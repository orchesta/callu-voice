package api

import (
	"fmt"
	"regexp"
	"strings"
)

// e164 is the only destination shape accepted. It also happens to forbid every
// character that could break out of the "PJSIP/%s@trunk" endpoint template
// (`@`, `/`, `&`, spaces), so it doubles as injection defence.
var e164 = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)

// validDTMF is one key press: a single digit, star or hash.
var validDTMF = regexp.MustCompile(`^[0-9*#]$`)

// validateDestination enforces E.164 shape and a default-deny prefix allowlist.
// E.164 alone permits premium and satellite ranges, so the allowlist is the cost control; an empty one rejects everything.
func validateDestination(to string, allowedPrefixes []string) error {
	if !e164.MatchString(to) {
		return fmt.Errorf("destination must be E.164 (e.g. +905321234567)")
	}
	if len(allowedPrefixes) == 0 {
		return fmt.Errorf("no destination prefixes are allowed; set ALLOWED_PREFIXES")
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(to, prefix) {
			return nil
		}
	}
	return fmt.Errorf("destination %s is not in the allowed prefixes", to)
}

// validateKeys rejects multi-character or colliding DTMF keys — neither can match a keypress unambiguously.
func validateKeys(keys ...string) error {
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !validDTMF.MatchString(k) {
			return fmt.Errorf("key %q must be a single DTMF character (0-9, *, #)", k)
		}
		if seen[k] {
			return fmt.Errorf("key %q is assigned to more than one action", k)
		}
		seen[k] = true
	}
	return nil
}
