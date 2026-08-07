package config

import "testing"

func TestTrunkFromFormat(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   string
	}{
		{"PJSIP/%s@trunk", "trunk"},
		{"PJSIP/%s@carrier-a", "carrier-a"},
		{"PJSIP/%s@ trunk ", "trunk"},
		// No "@" means the format dials something that is not a trunk, so there is nothing to
		// watch — and reporting an invented endpoint as offline would fail readiness forever.
		{"PJSIP/%s", ""},
		{"Local/%s@default", "default"},
		{"", ""},
	} {
		if got := trunkFromFormat(tc.format); got != tc.want {
			t.Errorf("trunkFromFormat(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}

// A shipped default like "change-me" is worse than an empty one: it looks configured. Every entry
// has to be reachable by the lookup, which lowercases what it is given.
func TestEveryPlaceholderIsMatchable(t *testing.T) {
	for value := range placeholders {
		if err := requireSecret("SECRET", value, 1); err == nil {
			t.Errorf("placeholder %q was accepted as a real secret", value)
		}
	}
}

func TestRequireSecretRejectsShortAndEmptyValues(t *testing.T) {
	if err := requireSecret("API_TOKEN", "", 16); err == nil {
		t.Error("an empty secret was accepted")
	}
	if err := requireSecret("API_TOKEN", "short", 16); err == nil {
		t.Error("a secret below the minimum length was accepted")
	}
	if err := requireSecret("API_TOKEN", "a-real-looking-secret-value", 16); err != nil {
		t.Errorf("a valid secret was rejected: %v", err)
	}
}

// The check lowercases, so a placeholder typed in capitals must not slip through.
func TestPlaceholderCheckIgnoresCase(t *testing.T) {
	if err := requireSecret("ARI_PASSWORD", "CHANGE-ME", 1); err == nil {
		t.Error("a capitalised placeholder was accepted")
	}
}

func TestSplitPrefixesTrimsAndDropsEmpties(t *testing.T) {
	got := splitPrefixes(" +90 , ,+1, ")

	if len(got) != 2 || got[0] != "+90" || got[1] != "+1" {
		t.Fatalf("splitPrefixes = %#v, want [+90 +1]", got)
	}
	// Empty must stay empty: the allowlist fails closed, and one stray entry would open it.
	if len(splitPrefixes("")) != 0 || len(splitPrefixes(" , ")) != 0 {
		t.Error("a blank ALLOWED_PREFIXES produced a non-empty allowlist")
	}
}
