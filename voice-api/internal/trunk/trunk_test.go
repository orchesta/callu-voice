package trunk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func enabled() Settings {
	return Settings{
		Enabled: true, Host: "sip.carrier.example", Port: 5060,
		Username: "900001", Password: "s3cr3t", Register: yes(),
	}
}

func TestValidateRefusesConfigInjection(t *testing.T) {
	// Asterisk config is line-oriented and unquoted, so a newline in a value writes new directives.
	for _, field := range []string{"host", "username", "password", "caller_id"} {
		s := enabled()
		switch field {
		case "host":
			s.Host = "sip.example\n[evil]\ntype=endpoint"
		case "username":
			s.Username = "u\npassword=hijacked"
		case "password":
			s.Password = "p\n[trunk-auth]\npassword=hijacked"
		case "caller_id":
			s.CallerID = "cid;type=auth"
		}
		if err := s.Validate(); err == nil {
			t.Fatalf("%s accepted a value carrying config syntax", field)
		}
	}
}

// Asterisk reads a config value as a C string, so everything past a NUL is dropped: the
// daemon would authenticate with a prefix of the password and 401 forever.
func TestValidateRefusesControlCharacters(t *testing.T) {
	for name, mutate := range map[string]func(*Settings){
		"nul in password":    func(s *Settings) { s.Password = "abc\x00def" },
		"tab in username":    func(s *Settings) { s.Username = "u\tv" },
		"nul in host":        func(s *Settings) { s.Host = "sip.example\x00.evil" },
		"tab in caller id":   func(s *Settings) { s.CallerID = "900\t001" },
		"escape in password": func(s *Settings) { s.Password = "a\x1bb" },
	} {
		s := enabled()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: accepted a control character Asterisk cannot carry", name)
		}
	}
}

// An over-long value is split by Asterisk's config reader, and the tail becomes a directive.
func TestValidateBoundsEveryFieldAtItsColumn(t *testing.T) {
	for name, mutate := range map[string]func(*Settings){
		"host":          func(s *Settings) { s.Host = strings.Repeat("h", 256) },
		"username":      func(s *Settings) { s.Username = strings.Repeat("u", 101) },
		"password":      func(s *Settings) { s.Password = strings.Repeat("p", 501) },
		"auth_username": func(s *Settings) { s.AuthUsername = strings.Repeat("a", 101) },
		"caller_id":     func(s *Settings) { s.CallerID = strings.Repeat("c", 51) },
	} {
		s := enabled()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: accepted a value longer than the column that feeds it", name)
		}
	}
}

func TestValidateRequiresWhatTheTrunkCannotWorkWithout(t *testing.T) {
	for name, mutate := range map[string]func(*Settings){
		"host":      func(s *Settings) { s.Host = "" },
		"username":  func(s *Settings) { s.Username = "" },
		"password":  func(s *Settings) { s.Password = "" },
		"port":      func(s *Settings) { s.Port = 70000 },
		"dtmf":      func(s *Settings) { s.DTMFMode = "shout" },
		"codecs":    func(s *Settings) { s.Codecs = "ulaw;alaw" },
		"transport": func(s *Settings) { s.Transport = "sctp" },
	} {
		s := enabled()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: accepted a value that cannot work", name)
		}
	}
}

// A host carrying its own port silently disagrees with the port field.
func TestValidateRefusesAHostWithAPort(t *testing.T) {
	s := enabled()
	s.Host = "sip.carrier.example:5061"
	if err := s.Validate(); err == nil {
		t.Fatal("a host with a port was accepted")
	}
}

// A bare IPv6 address renders sip:2001:db8::1:5060, which no carrier can parse; the bracketed
// form is the only one that works, so it is the one that must be accepted.
func TestValidateTakesIPv6InBracketsAndRefusesItBare(t *testing.T) {
	s := enabled()
	s.Host = "[2001:db8::1]"
	if err := s.Validate(); err != nil {
		t.Fatalf("a bracketed IPv6 host was refused: %v", err)
	}
	if out := s.Render(); !strings.Contains(out, "contact=sip:[2001:db8::1]:5060") {
		t.Fatalf("the bracketed host did not survive into the contact: %q", out)
	}
	if out := s.Render(); !strings.Contains(out, "match=2001:db8::1\n") {
		t.Fatalf("identify match should carry the address without brackets: %q", out)
	}

	for _, bad := range []string{"2001:db8::1", "fe80::1%eth0", "[2001:db8::1", "[sip.example]"} {
		s := enabled()
		s.Host = bad
		if err := s.Validate(); err == nil {
			t.Fatalf("%q was accepted and would render a URI nothing can dial", bad)
		}
	}
}

// pjsip.conf binds no TLS transport, so accepting tls would send the credentials in the clear
// while the caller believes they are encrypted.
func TestValidateRefusesTLSRatherThanDowngradingIt(t *testing.T) {
	s := enabled()
	s.Transport = "tls"
	err := s.Validate()
	if err == nil {
		t.Fatal("tls was accepted and would have been rendered as plaintext udp")
	}
	if !strings.Contains(err.Error(), "tls") {
		t.Fatalf("the refusal does not name the transport it refused: %v", err)
	}
}

func TestRenderCarriesTheSelectedTransport(t *testing.T) {
	s := enabled()
	s.Transport = "tcp"
	out := s.Render()
	if strings.Contains(out, "transport=transport-udp") {
		t.Fatal("a tcp trunk was rendered over udp")
	}
	if !strings.Contains(out, "transport=transport-tcp") {
		t.Fatalf("the tcp transport did not reach the endpoint: %q", out)
	}
}

// A pasted hostname commonly carries a trailing space; rendered untrimmed it produces an AOR
// with no usable contact, and Asterisk reloads it without complaint.
func TestValuesAreTrimmedBeforeTheyAreWritten(t *testing.T) {
	s := enabled()
	s.Host = "  sip.carrier.example  "
	s.Password = "  s3cr3t  "
	if err := s.Validate(); err != nil {
		t.Fatalf("a padded host was refused outright: %v", err)
	}
	out := s.Render()
	if !strings.Contains(out, "contact=sip:sip.carrier.example:5060") {
		t.Fatalf("the host was written with its padding: %q", out)
	}
	if !strings.Contains(out, "password=s3cr3t\n") {
		t.Fatalf("the password was written with its padding: %q", out)
	}
}

func TestADisabledTrunkNeedsNothingAndRendersNothing(t *testing.T) {
	s := Settings{Enabled: false}
	if err := s.Validate(); err != nil {
		t.Fatalf("a disabled trunk was refused: %v", err)
	}
	if strings.Contains(s.Render(), "[trunk]") {
		t.Fatal("a disabled trunk rendered an endpoint")
	}
}

func TestRenderCarriesTheCarrierIntoEverySectionThatNeedsIt(t *testing.T) {
	out := enabled().Render()
	for _, want := range []string{
		"[trunk]", "[trunk-auth]", "[trunk-aor]", "[trunk-identify]", "[trunk-registration]",
		"contact=sip:sip.carrier.example:5060",
		"username=900001",
		"password=s3cr3t",
		"match=sip.carrier.example",
		"client_uri=sip:900001@sip.carrier.example\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config is missing %q", want)
		}
	}
}

// An IP-authenticated carrier must not be sent a registration.
func TestRegistrationIsOnlyWrittenWhenAskedFor(t *testing.T) {
	s := enabled()
	s.Register = no()
	if strings.Contains(s.Render(), "[trunk-registration]") {
		t.Fatal("a registration was written for a trunk that did not ask for one")
	}
}

// There is no environment left to take these from, so the caller that states nothing has to get
// something that works: telephone-event with inband fallback, the two codecs every carrier
// offers, and a registration.
func TestUnstatedFieldsTakeTheServiceDefaults(t *testing.T) {
	s := enabled()
	s.Register = nil
	s.DTMFMode = ""
	s.Codecs = ""

	out := s.Render()
	if !strings.Contains(out, "dtmf_mode=auto") {
		t.Fatalf("an unstated dtmf mode did not default to auto: %q", out)
	}
	if !strings.Contains(out, "allow=ulaw,alaw") {
		t.Fatalf("an unstated codec list did not default to ulaw,alaw: %q", out)
	}
	if !strings.Contains(out, "[trunk-registration]") {
		t.Fatalf("an unstated register did not default to registering: %q", out)
	}
}

// The whole of reconciliation rests on this: the caller compares its own answer against the one
// GET /trunk reports, so the same carrier has to hash the same both times.
func TestTheFingerprintIsStableForTheSameCarrier(t *testing.T) {
	stated := enabled()
	stated.Transport = "udp"
	first := stated.Fingerprint()

	padded := stated
	padded.Host = "  sip.carrier.example  "
	padded.Password = " s3cr3t "
	padded.Transport = "UDP"

	if padded.Fingerprint() != first {
		t.Fatal("padding a value changed the fingerprint of the same carrier")
	}

	// Nothing the caller does not state is in it, so a service default cannot move it.
	unstated := stated
	unstated.Register = no()
	unstated.DTMFMode = "info"
	unstated.Codecs = "g722"
	if unstated.Fingerprint() != first {
		t.Fatal("a field the caller does not own moved the fingerprint")
	}
}

// Each of these is a carrier that answers differently — one that goes unnoticed is a page that
// leaves through the wrong trunk, or through none.
func TestEveryFieldTheCallerOwnsChangesTheFingerprint(t *testing.T) {
	base := enabled().Fingerprint()

	for name, mutate := range map[string]func(*Settings){
		"host":          func(s *Settings) { s.Host = "sip.other.example" },
		"port":          func(s *Settings) { s.Port = 5080 },
		"username":      func(s *Settings) { s.Username = "900002" },
		"password":      func(s *Settings) { s.Password = "rotated" },
		"auth_username": func(s *Settings) { s.AuthUsername = "900003" },
		"caller_id":     func(s *Settings) { s.CallerID = "+905551112233" },
		"transport":     func(s *Settings) { s.Transport = "tcp" },
		"enabled":       func(s *Settings) { s.Enabled = false },
	} {
		s := enabled()
		mutate(&s)
		if s.Fingerprint() == base {
			t.Fatalf("changing %s left the fingerprint unchanged", name)
		}
	}
}

// It travels over the wire on an unencrypted internal hop and lands in the caller's logs when a
// push is recorded, so it must not be reversible into the carrier's password.
func TestTheFingerprintDiscloseNoPassword(t *testing.T) {
	s := enabled()
	print := s.Fingerprint()
	if strings.Contains(print, "s3cr3t") || strings.Contains(print, s.Host) {
		t.Fatalf("the fingerprint carries the settings in the clear: %s", print)
	}
	if len(print) != 64 {
		t.Fatalf("the fingerprint is not a sha256 digest: %s", print)
	}
}

func managerFor(t *testing.T, reload func(context.Context) error) (*Manager, string) {
	t.Helper()
	return managerWith(t, reload, func(context.Context) (string, error) {
		return "Endpoint:  trunk   Not in use   0 of inf", nil
	})
}

func managerWith(
	t *testing.T,
	reload func(context.Context) error,
	confirm func(context.Context) (string, error),
) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pjsip_trunk.conf")
	if err := os.WriteFile(path, []byte("; before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	m.reload = reload
	m.confirm = confirm
	return m, path
}

func TestApplyWritesAndReloads(t *testing.T) {
	reloaded := 0
	m, path := managerFor(t, func(context.Context) error { reloaded++; return nil })

	state, err := m.Apply(context.Background(), enabled())
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if reloaded != 1 {
		t.Fatalf("expected one reload, got %d", reloaded)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "sip.carrier.example") {
		t.Fatal("the carrier did not reach the config file")
	}
	if !state.HasPassword || state.Host != "sip.carrier.example" {
		t.Fatalf("state does not describe what was applied: %+v", state)
	}
}

// A config Asterisk will not load must not be left behind: the next reload — a restart, or
// another carrier change — would adopt it silently.
func TestAFailedReloadPutsThePreviousConfigBack(t *testing.T) {
	m, path := managerFor(t, func(context.Context) error { return errors.New("asterisk said no") })

	if _, err := m.Apply(context.Background(), enabled()); err == nil {
		t.Fatal("apply reported success though the reload failed")
	}

	body, _ := os.ReadFile(path)
	if string(body) != "; before\n" {
		t.Fatalf("the rejected config was left in place: %q", body)
	}
}

// The reload the rollback needs is the one most likely to be cancelled, because the caller has
// usually given up by then — and a rollback Asterisk never hears about leaves the running
// daemon on a carrier the file no longer names.
func TestTheRollbackReloadRunsEvenWhenTheCallerHasGoneAway(t *testing.T) {
	live := 0
	m, path := managerFor(t, func(ctx context.Context) error {
		if ctx.Err() == nil {
			live++
		}
		return errors.New("asterisk said no")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Apply(ctx, enabled()); err == nil {
		t.Fatal("apply reported success though the reload failed")
	}
	if live == 0 {
		t.Fatal("the rollback reload was never attempted with a usable context")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "; before\n" {
		t.Fatalf("the rejected config was left in place: %q", body)
	}
}

// module reload exits 0 while refusing objects it could not build, so a successful reload is
// not evidence the trunk is there — and a trunk that is not there pages nobody.
func TestAReloadThatBuiltNoEndpointIsNotSuccess(t *testing.T) {
	reloads := 0
	m, path := managerWith(t,
		func(context.Context) error { reloads++; return nil },
		func(context.Context) (string, error) { return "Unable to find object trunk.", nil })

	if _, err := m.Apply(context.Background(), enabled()); err == nil {
		t.Fatal("apply reported success though Asterisk built no trunk endpoint")
	}
	if reloads != 2 {
		t.Fatalf("expected the rollback to reload as well, got %d reloads", reloads)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "; before\n" {
		t.Fatalf("a config Asterisk would not build was left in place: %q", body)
	}
}

// Turning the carrier off deletes the endpoint on purpose, so its absence is the success case.
func TestADisabledTrunkIsNotCheckedForAnEndpoint(t *testing.T) {
	m, _ := managerWith(t,
		func(context.Context) error { return nil },
		func(context.Context) (string, error) { return "Unable to find object trunk.", nil })

	if _, err := m.Apply(context.Background(), Settings{Enabled: false}); err != nil {
		t.Fatalf("turning the carrier off was reported as a failure: %v", err)
	}
}

// The password is the one field a caller must never be handed back.
func TestStateNeverCarriesThePassword(t *testing.T) {
	m, _ := managerFor(t, func(context.Context) error { return nil })
	state, err := m.Apply(context.Background(), enabled())
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasPassword {
		t.Fatal("state does not report that a password is stored")
	}
	if strings.Contains(strings.ToLower(state.Host+state.Username+state.CallerID), "s3cr3t") {
		t.Fatal("the password leaked into the readable state")
	}
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "s3cr3t") {
		t.Fatalf("the password leaked into the encoded state: %s", body)
	}
}

// This service keeps no carrier of its own: a fresh process reports none, and the caller's
// reconciliation is what puts one back. Answering from a file left behind would be a second
// source of truth that nobody can see.
func TestAFreshProcessReportsNoCarrier(t *testing.T) {
	m, path := managerFor(t, func(context.Context) error { return nil })
	if _, err := m.Apply(context.Background(), enabled()); err != nil {
		t.Fatal(err)
	}

	state := NewManager(path).Current()
	if state.Enabled || state.Host != "" || state.HasPassword {
		t.Fatalf("a fresh process reports a carrier it was never given: %+v", state)
	}
	if state.Fingerprint != (Settings{}).Fingerprint() {
		t.Fatal("a fresh process does not report the fingerprint of having no carrier")
	}
}

// Nothing downstream can tell a carrier removed on purpose from one that stopped answering
// unless what applied the change says so.
func TestApplyTellsTheWatcherWhatWasPutInPlace(t *testing.T) {
	var told []bool
	m, _ := managerFor(t, func(context.Context) error { return nil })
	m.Watch(func(configured bool) { told = append(told, configured) })

	if _, err := m.Apply(context.Background(), enabled()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(context.Background(), Settings{Enabled: false}); err != nil {
		t.Fatal(err)
	}

	if len(told) != 2 || !told[0] || told[1] {
		t.Fatalf("the watcher was not told what was applied: %v", told)
	}
}

// A carrier Asterisk refused is not in place, and reporting it as one would clear a verdict that
// is still true.
func TestAnApplyThatFailedTellsTheWatcherNothing(t *testing.T) {
	told := 0
	m, _ := managerFor(t, func(context.Context) error { return errors.New("asterisk said no") })
	m.Watch(func(bool) { told++ })

	if _, err := m.Apply(context.Background(), enabled()); err == nil {
		t.Fatal("apply reported success though the reload failed")
	}
	if told != 0 {
		t.Fatalf("a carrier that never reached Asterisk was announced %d time(s)", told)
	}
}

// Only the config file, and only where pjsip.conf reads it. Anything else beside it outlives a
// reload nobody performed and starts disagreeing with the daemon.
func TestApplyWritesNothingBesideTheConfigFile(t *testing.T) {
	m, path := managerFor(t, func(context.Context) error { return nil })
	if _, err := m.Apply(context.Background(), enabled()); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(path) {
			t.Fatalf("apply left %q beside the trunk config", entry.Name())
		}
	}
}

func TestInvalidSettingsNeverTouchTheFile(t *testing.T) {
	reloaded := 0
	m, path := managerFor(t, func(context.Context) error { reloaded++; return nil })

	s := enabled()
	s.Host = ""
	if _, err := m.Apply(context.Background(), s); err == nil {
		t.Fatal("an invalid trunk was applied")
	}
	if reloaded != 0 {
		t.Fatal("Asterisk was reloaded for a config that was never written")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "; before\n" {
		t.Fatal("the file was rewritten for invalid settings")
	}
}

// A confirmation that says nothing at all is not a confirmation.
func TestAnEmptyConfirmationIsNotTreatedAsALoadedEndpoint(t *testing.T) {
	m, path := managerWith(t,
		func(context.Context) error { return nil },
		func(context.Context) (string, error) { return "  \n", nil })

	if _, err := m.Apply(context.Background(), enabled()); err == nil {
		t.Fatal("apply reported success though Asterisk confirmed nothing")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "; before\n" {
		t.Fatalf("an unconfirmed config was left in place: %q", body)
	}
}
