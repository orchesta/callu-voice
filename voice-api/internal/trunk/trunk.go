// Package trunk renders the SIP trunk half of Asterisk's PJSIP configuration and reloads it,
// so a carrier can be changed without recreating the container.
package trunk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What a caller that states nothing gets. There is no environment to take these from: this
// service holds no carrier of its own, and the caller supplies the whole of one.
const (
	defaultPort      = 5060
	defaultTransport = "udp"
	defaultRegister  = true
	defaultDTMFMode  = "auto"
	defaultCodecs    = "ulaw,alaw"
)

// Settings is one carrier, as the caller describes it.
type Settings struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Defaults to Username when blank, which is what most carriers expect.
	AuthUsername string `json:"auth_username"`
	CallerID     string `json:"caller_id"`
	// udp | tcp. pjsip.conf binds no TLS transport, so tls is refused rather than downgraded.
	Transport string `json:"transport"`
	// Absent registers, which is what most carriers expect.
	Register *bool `json:"register"`
	// rfc4733 | inband | info | auto | auto_info | none
	DTMFMode string `json:"dtmf_mode"`
	Codecs   string `json:"codecs"`
}

// State is what a caller may read back. It never carries the password.
type State struct {
	Enabled      bool   `json:"enabled"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Username     string `json:"username"`
	AuthUsername string `json:"auth_username"`
	CallerID     string `json:"caller_id"`
	Transport    string `json:"transport"`
	Register     bool   `json:"register"`
	DTMFMode     string `json:"dtmf_mode"`
	Codecs       string `json:"codecs"`
	HasPassword  bool   `json:"has_password"`
	// Identifies the carrier in force without disclosing it, so the caller can compare it
	// against its own record. Empty until something has been applied.
	Fingerprint string    `json:"fingerprint"`
	AppliedAt   time.Time `json:"applied_at"`
}

// Version tag, so a change to what the fingerprint covers cannot be mistaken for a match.
const fingerprintVersion = "callu-voice/trunk/v1"

// Fingerprint identifies a carrier without disclosing it.
//
// It covers the fields the caller states, normalized and with the password hashed first —
// never the values this service fills in for what the caller left out, because the caller
// cannot know those and would compute a different answer for the same carrier forever.
func (s Settings) Fingerprint() string {
	s = s.normalize()
	if !s.Enabled {
		return digest(fingerprintVersion + "\nenabled=false")
	}
	return digest(strings.Join([]string{
		fingerprintVersion,
		"enabled=true",
		"host=" + s.Host,
		"port=" + strconv.Itoa(s.Port),
		"username=" + s.Username,
		"auth_username=" + s.AuthUsername,
		"caller_id=" + s.CallerID,
		"transport=" + s.Transport,
		"password=" + digest(s.Password),
	}, "\n"))
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

var (
	validDTMF   = map[string]bool{"rfc4733": true, "inband": true, "info": true, "auto": true, "auto_info": true, "none": true}
	validCodecs = regexp.MustCompile(`^[a-z0-9]+(,[a-z0-9]+)*$`)
	// Asterisk config is line-oriented and unquoted, so a newline in a value is a config
	// injection; it reads a value as a C string, so a NUL truncates one.
	forbidden = regexp.MustCompile("[\\x00-\\x1f\\x7f\\[\\];]")
	// Each field is bounded at the column that feeds it, so an over-long line cannot become
	// a directive of its own.
	maxLength = map[string]int{
		"host": 255, "username": 100, "password": 500, "auth_username": 100, "caller_id": 50,
	}
)

// Validate refuses anything that would produce a broken or forged config file.
func (s Settings) Validate() error {
	s = s.normalize()
	if !s.Enabled {
		return nil
	}
	for name, value := range map[string]string{
		"username": s.Username, "password": s.Password,
		"auth_username": s.AuthUsername, "caller_id": s.CallerID,
	} {
		if forbidden.MatchString(value) {
			return fmt.Errorf("%s contains a character that cannot appear in Asterisk configuration", name)
		}
	}
	for name, value := range map[string]string{
		"host": s.Host, "username": s.Username, "password": s.Password,
		"auth_username": s.AuthUsername, "caller_id": s.CallerID,
	} {
		if limit := maxLength[name]; len(value) > limit {
			return fmt.Errorf("%s must be at most %d characters", name, limit)
		}
	}
	if err := validateHost(s.Host); err != nil {
		return err
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if s.Username == "" {
		return fmt.Errorf("username is required when the trunk is enabled")
	}
	if s.Password == "" {
		return fmt.Errorf("password is required when the trunk is enabled")
	}
	switch s.Transport {
	case "", "udp", "tcp":
	case "tls":
		return fmt.Errorf("transport tls is not supported: this service binds SIP over UDP and TCP only")
	default:
		return fmt.Errorf("transport must be udp or tcp")
	}
	if s.DTMFMode != "" && !validDTMF[s.DTMFMode] {
		return fmt.Errorf("dtmf_mode must be one of rfc4733, inband, info, auto, auto_info, none")
	}
	if s.Codecs != "" && !validCodecs.MatchString(s.Codecs) {
		return fmt.Errorf("codecs must be a comma-separated list, e.g. ulaw,alaw")
	}
	return nil
}

// validateHost accepts a name or an IPv4 address bare, and an IPv6 address only in the brackets
// a SIP URI needs — the bare form renders a URI no carrier can parse.
func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("host is required when the trunk is enabled")
	}
	if inner, bracketed := strings.CutPrefix(host, "["); bracketed {
		address, closed := strings.CutSuffix(inner, "]")
		if !closed || net.ParseIP(address) == nil || !strings.Contains(address, ":") {
			return fmt.Errorf("a bracketed host must be an IPv6 address, e.g. [2001:db8::1]")
		}
		return nil
	}
	if forbidden.MatchString(host) {
		return fmt.Errorf("host contains a character that cannot appear in Asterisk configuration")
	}
	if strings.Count(host, ":") > 1 {
		return fmt.Errorf("an IPv6 host must be written in brackets, e.g. [2001:db8::1]")
	}
	if name, _, err := net.SplitHostPort(host); err == nil && name != "" {
		return fmt.Errorf("host must not carry a port; use the port field")
	}
	return nil
}

// normalize strips the whitespace Asterisk would strip anyway, so what is stored, what is
// written and what the daemon authenticates with are the same string.
func (s Settings) normalize() Settings {
	s.Host = strings.TrimSpace(s.Host)
	s.Username = strings.TrimSpace(s.Username)
	s.Password = strings.TrimSpace(s.Password)
	s.AuthUsername = strings.TrimSpace(s.AuthUsername)
	s.CallerID = strings.TrimSpace(s.CallerID)
	s.Transport = strings.ToLower(strings.TrimSpace(s.Transport))
	s.DTMFMode = strings.ToLower(strings.TrimSpace(s.DTMFMode))
	s.Codecs = strings.ToLower(strings.TrimSpace(s.Codecs))
	return s
}

func (s Settings) withDefaults() Settings {
	s = s.normalize()
	if s.Port == 0 {
		s.Port = defaultPort
	}
	if s.AuthUsername == "" {
		s.AuthUsername = s.Username
	}
	if s.CallerID == "" {
		s.CallerID = s.Username
	}
	if s.Transport == "" {
		s.Transport = defaultTransport
	}
	if s.Register == nil {
		on := defaultRegister
		s.Register = &on
	}
	if s.DTMFMode == "" {
		s.DTMFMode = defaultDTMFMode
	}
	if s.Codecs == "" {
		s.Codecs = defaultCodecs
	}
	return s
}

// Render produces the PJSIP sections for this trunk.
func (s Settings) Render() string {
	if !s.Enabled {
		return "; no trunk configured\n"
	}
	s = s.withDefaults()

	var b strings.Builder
	fmt.Fprintf(&b, `[trunk]
type=endpoint
transport=transport-%s
context=from-trunk
disallow=all
allow=%s
dtmf_mode=%s
outbound_auth=trunk-auth
aors=trunk-aor
from_user=%s
callerid=%s
direct_media=no
rtp_symmetric=yes
force_rport=yes
rewrite_contact=yes

[trunk-auth]
type=auth
auth_type=userpass
username=%s
password=%s

[trunk-aor]
type=aor
contact=sip:%s:%d
qualify_frequency=60

[trunk-identify]
type=identify
endpoint=trunk
match=%s
`, s.Transport, s.Codecs, s.DTMFMode, s.Username, s.CallerID, s.AuthUsername, s.Password,
		s.Host, s.Port, matchHost(s.Host))

	if *s.Register {
		fmt.Fprintf(&b, `
[trunk-registration]
type=registration
transport=transport-%s
outbound_auth=trunk-auth
server_uri=sip:%s:%d
client_uri=sip:%s@%s
retry_interval=60
forbidden_retry_interval=600
expiration=3600
`, s.Transport, s.Host, s.Port, s.Username, s.Host)
	}
	return b.String()
}

// matchHost is the identify match, which takes an address rather than a URI host.
func matchHost(host string) string { return strings.Trim(host, "[]") }

// state describes what was applied; the fingerprint is taken over what the caller stated.
func (s Settings) state(fingerprint string, appliedAt time.Time) State {
	r := s.withDefaults()
	return State{
		Enabled: s.Enabled, Host: r.Host, Port: r.Port, Username: r.Username,
		AuthUsername: r.AuthUsername, CallerID: r.CallerID, Transport: r.Transport,
		Register: *r.Register, DTMFMode: r.DTMFMode, Codecs: r.Codecs,
		HasPassword: r.Password != "", Fingerprint: fingerprint, AppliedAt: appliedAt,
	}
}

// ConfigPath is where pjsip.conf expects the trunk half to live; entrypoint.sh writes the same
// file empty at boot.
const ConfigPath = "/etc/asterisk/pjsip_trunk.conf"

// Manager owns the trunk config file and the reload that makes Asterisk read it.
type Manager struct {
	path    string
	reload  func(context.Context) error
	confirm func(context.Context) (string, error)

	observer func(configured bool)

	mu      sync.RWMutex
	current State
}

// Watch registers what to tell when a carrier is applied or removed, which is the only way
// anything else can tell a carrier taken away on purpose from one that stopped answering.
func (m *Manager) Watch(observer func(configured bool)) { m.observer = observer }

// NewManager writes to path and reloads PJSIP by asking the running Asterisk to.
//
// It starts holding no carrier, and keeps none across a restart: the caller reconciles what
// this service reports against its own record and sends the carrier again when they differ.
func NewManager(path string) *Manager {
	return &Manager{
		path:    path,
		reload:  reloadPJSIP,
		confirm: showTrunkEndpoint,
		current: State{Fingerprint: Settings{}.Fingerprint()},
	}
}

// Apply validates, writes and reloads. Nothing is left half-applied: the file is replaced
// atomically, and a config Asterisk would not build puts the previous file back.
func (m *Manager) Apply(ctx context.Context, s Settings) (State, error) {
	state, err := m.apply(ctx, s)
	if err != nil {
		return State{}, err
	}
	// Told after the lock is released, and only for a carrier that really reached Asterisk.
	if m.observer != nil {
		m.observer(state.Enabled)
	}
	return state, nil
}

func (m *Manager) apply(ctx context.Context, s Settings) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	fingerprint := s.Fingerprint()
	s = s.withDefaults()

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, readErr := os.ReadFile(m.path)

	if err := m.writeAtomic(m.path, s.Render()); err != nil {
		return State{}, fmt.Errorf("writing trunk configuration: %w", err)
	}

	if err := m.applyToAsterisk(ctx, s); err != nil {
		return State{}, m.rollback(ctx, previous, readErr, err)
	}

	m.current = s.state(fingerprint, time.Now().UTC())
	return m.current, nil
}

// applyToAsterisk reloads and then checks the endpoint is really there: the module reload
// reports success while refusing individual objects it could not build.
func (m *Manager) applyToAsterisk(ctx context.Context, s Settings) error {
	if err := m.reload(ctx); err != nil {
		return fmt.Errorf("reloading Asterisk: %w", err)
	}
	if !s.Enabled {
		return nil
	}
	out, err := m.confirm(ctx)
	if err != nil {
		return fmt.Errorf("checking the trunk endpoint after the reload: %w", err)
	}
	if !endpointLoaded(out) {
		return fmt.Errorf("Asterisk reloaded but did not build the trunk endpoint: %s", oneLine(out))
	}
	return nil
}

// rollback restores the file the reload rejected and says so in the returned error, which is
// the only place a caller can learn the previous carrier is not back either.
func (m *Manager) rollback(ctx context.Context, previous []byte, readErr, cause error) error {
	if readErr != nil {
		return fmt.Errorf("%w (the previous configuration could not be read, so it was not put back: %v)", cause, readErr)
	}
	// Detached: the caller may already have given up, and a rollback that never reaches Asterisk
	// leaves the daemon and the file disagreeing.
	restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()

	if err := m.writeAtomic(m.path, string(previous)); err != nil {
		return fmt.Errorf("%w (the previous configuration could not be put back: %v)", cause, err)
	}
	if err := m.reload(restore); err != nil {
		return fmt.Errorf("%w (the previous configuration was put back but Asterisk did not reload it: %v)", cause, err)
	}
	return cause
}

// Current is the trunk this service last applied, without its password.
func (m *Manager) Current() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *Manager) writeAtomic(path, contents string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pjsip_trunk-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

const (
	cliTimeout     = 15 * time.Second
	restoreTimeout = 15 * time.Second
)

// reloadPJSIP asks the running Asterisk to re-read its PJSIP configuration. Reloading the
// module rather than restarting keeps calls that are already up.
func reloadPJSIP(ctx context.Context) error {
	_, err := asteriskCLI(ctx, "module reload res_pjsip.so")
	return err
}

// showTrunkEndpoint reports what Asterisk built, which is not what it was asked to build.
func showTrunkEndpoint(ctx context.Context) (string, error) {
	return asteriskCLI(ctx, "pjsip show endpoint trunk")
}

func asteriskCLI(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "asterisk", "-rx", command).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%w: %s", err, text)
	}
	// The CLI exits 0 even when it never reached the daemon, so the answer has to be read.
	lower := strings.ToLower(text)
	if strings.Contains(lower, "unable to connect") || strings.Contains(lower, "no such command") {
		return text, fmt.Errorf("asterisk did not accept %q: %s", command, text)
	}
	return text, nil
}

// endpointLoaded reads the CLI's answer for the one thing it says plainly when a reload was
// accepted but the object was not built: it cannot find it.
func endpointLoaded(showOutput string) bool {
	text := strings.TrimSpace(showOutput)
	if text == "" {
		return false
	}
	return !strings.Contains(strings.ToLower(text), "unable to find")
}

func oneLine(text string) string {
	fields := strings.Fields(text)
	line := strings.Join(fields, " ")
	if len(line) > 200 {
		return line[:200]
	}
	return line
}
