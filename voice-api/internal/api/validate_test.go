package api

import "testing"

func TestValidateDestination(t *testing.T) {
	allow := []string{"+90", "+1"}

	cases := []struct {
		name string
		to   string
		ok   bool
	}{
		{"allowed turkish mobile", "+905321234567", true},
		{"allowed us", "+15551234567", true},
		{"not e164 - no plus", "905321234567", false},
		{"not e164 - letters", "+90ABC1234567", false},
		{"premium not in allowlist", "+8821234567890", false},
		{"us premium 1900 is allowed by prefix +1 (allowlist must be specific)", "+19001234567", true},
		{"injection via at", "+90@evil/trunk", false},
		{"injection via ampersand", "+90&SIP/x", false},
		{"empty", "", false},
		{"too short", "+9012", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateDestination(c.to, allow)
			if (err == nil) != c.ok {
				t.Fatalf("validateDestination(%q) err=%v, want ok=%v", c.to, err, c.ok)
			}
		})
	}
}

func TestValidateDestinationFailsClosed(t *testing.T) {
	// An empty allowlist must reject everything — a misconfiguration cannot
	// silently permit the world.
	if err := validateDestination("+905321234567", nil); err == nil {
		t.Fatal("empty allowlist accepted a destination; must fail closed")
	}
}

func TestValidateKeys(t *testing.T) {
	if err := validateKeys("1", "2", "*", "9"); err != nil {
		t.Fatalf("valid distinct single-char keys rejected: %v", err)
	}
	if err := validateKeys("1", "1", "*", "9"); err == nil {
		t.Fatal("colliding keys accepted")
	}
	if err := validateKeys("12", "2", "*", "9"); err == nil {
		t.Fatal("multi-character key accepted; it would match no DTMF event")
	}
	if err := validateKeys("1", "2", "x", "9"); err == nil {
		t.Fatal("non-DTMF key accepted")
	}
}
