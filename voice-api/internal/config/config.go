// Package config reads the service settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// placeholder values that must never reach production. A shipped default like
// "change-me" is worse than an empty one: it looks configured.
var placeholders = map[string]bool{
	"change-me": true, "changeme": true, "change": true,
	"password": true, "secret": true, "todo": true, "xxx": true,
}

type Config struct {
	HTTPAddr string

	// Shared secret the caller presents on POST /calls. Required; the endpoint
	// places real PSTN calls, so an unauthenticated path is toll fraud.
	APIToken string

	ARIApplication string
	ARIURL         string
	ARIWebsocket   string
	ARIUsername    string
	ARIPassword    string

	// How an outbound number becomes a channel, e.g. "PJSIP/%s@trunk".
	EndpointFormat string

	// PJSIP endpoint outbound calls leave through, taken from EndpointFormat and watched so a
	// carrier that stops answering is visible on /health. Empty means nothing is watched.
	TrunkEndpoint string

	// Outbound calls run this dialplan before entering Stasis, which is where
	// answering-machine detection happens.
	DialplanContext   string
	DialplanExtension string
	DetectMachine     bool
	CallerID          string
	DialTimeout       time.Duration

	TTSURL     string
	TTSTimeout time.Duration

	CallbackTimeout  time.Duration
	CallbackAttempts int
	CallbackBackoff  time.Duration
	// Allow callbacks to private addresses. Default true: Callu usually sits on a private network.
	// Loopback and link-local are blocked regardless of this.
	CallbackAllowPrivate bool

	// Cost and abuse controls.
	AllowedPrefixes    []string // default-deny E.164 prefix allowlist for destinations
	MaxConcurrentCalls int      // 0 = unlimited (not recommended)
	MaxRequestBytes    int64

	// Defaults applied, and ceilings clamped, when a request specifies limits.
	MaxDuration    time.Duration
	SilenceTimeout time.Duration
	MaxReprompts   int
	DefaultVoice   string
	// Language a segment is spoken in when the request omits one. Empty is not a
	// usable value downstream.
	DefaultLang string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:          env("HTTP_ADDR", ":8090"),
		APIToken:          os.Getenv("API_TOKEN"),
		ARIApplication:    env("ARI_APPLICATION", "callu-voice"),
		ARIURL:            env("ARI_URL", "http://127.0.0.1:8088/ari"),
		ARIWebsocket:      env("ARI_WEBSOCKET", "ws://127.0.0.1:8088/ari/events"),
		ARIUsername:       os.Getenv("ARI_USERNAME"),
		ARIPassword:       os.Getenv("ARI_PASSWORD"),
		EndpointFormat:    env("ENDPOINT_FORMAT", "PJSIP/%s@trunk"),
		TrunkEndpoint:     trunkFromFormat(env("ENDPOINT_FORMAT", "PJSIP/%s@trunk")),
		DialplanContext:   env("DIALPLAN_CONTEXT", "callu-outbound"),
		DialplanExtension: env("DIALPLAN_EXTENSION", "s"),
		DetectMachine:     env("DETECT_MACHINE", "false") == "true",
		CallerID:          os.Getenv("CALLER_ID"),
		TTSURL:            env("TTS_URL", "http://127.0.0.1:8080"),
		DefaultVoice:      env("DEFAULT_VOICE", "F1"),
		DefaultLang:       env("DEFAULT_LANG", "tr"),

		// Default true: the legitimate callback target is Callu, typically on a
		// private network. Loopback/link-local/metadata are blocked regardless.
		CallbackAllowPrivate: env("CALLBACK_ALLOW_PRIVATE", "true") == "true",
		AllowedPrefixes:      splitPrefixes(os.Getenv("ALLOWED_PREFIXES")),
	}

	var err error
	if c.TTSTimeout, err = duration("TTS_TIMEOUT", 60*time.Second); err != nil {
		return c, err
	}
	if c.CallbackTimeout, err = duration("CALLBACK_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.CallbackBackoff, err = duration("CALLBACK_BACKOFF", 2*time.Second); err != nil {
		return c, err
	}
	if c.MaxDuration, err = duration("MAX_CALL_DURATION", 120*time.Second); err != nil {
		return c, err
	}
	if c.SilenceTimeout, err = duration("SILENCE_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}
	if c.DialTimeout, err = duration("DIAL_TIMEOUT", 45*time.Second); err != nil {
		return c, err
	}
	if c.CallbackAttempts, err = integer("CALLBACK_ATTEMPTS", 3); err != nil {
		return c, err
	}
	if c.MaxReprompts, err = integer("MAX_REPROMPTS", 2); err != nil {
		return c, err
	}
	if c.MaxConcurrentCalls, err = integer("MAX_CONCURRENT_CALLS", 30); err != nil {
		return c, err
	}
	if c.MaxRequestBytes, err = integer64("MAX_REQUEST_BYTES", 256*1024); err != nil {
		return c, err
	}

	// --- required secrets, checked for placeholders ---
	if err := requireSecret("API_TOKEN", c.APIToken, 16); err != nil {
		return c, err
	}
	if c.ARIUsername == "" {
		return c, fmt.Errorf("ARI_USERNAME is required")
	}
	if err := requireSecret("ARI_PASSWORD", c.ARIPassword, 12); err != nil {
		return c, err
	}
	return c, nil
}

// requireSecret rejects empty, too-short, and obvious placeholder values so a
// misconfiguration fails at boot rather than shipping "change-me" to production.
func requireSecret(name, value string, minLen int) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if placeholders[strings.ToLower(value)] {
		return fmt.Errorf("%s is set to a placeholder value; use a real secret", name)
	}
	if len(value) < minLen {
		return fmt.Errorf("%s must be at least %d characters", name, minLen)
	}
	return nil
}

// trunkFromFormat reads the endpoint name out of a format like "PJSIP/%s@trunk". A format with no
// "@" dials something that is not a trunk, and there is no carrier to watch.
func trunkFromFormat(format string) string {
	at := strings.LastIndex(format, "@")
	if at < 0 {
		return ""
	}
	return strings.TrimSpace(format[at+1:])
}

func splitPrefixes(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func integer(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func integer64(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
