// Package api exposes the HTTP contract the caller uses to place a page.
//
// The contract deliberately mirrors what a cloud voice provider would offer, so
// the caller can swap providers without changing its own model.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orchesta/callu-voice/internal/call"
	"github.com/orchesta/callu-voice/internal/config"
	"github.com/orchesta/callu-voice/internal/trunk"
	"github.com/orchesta/callu-voice/internal/tts"
	"github.com/orchesta/callu-voice/internal/voice"
)

// Readiness answers whether the service can actually place a call right now.
// A paging service that reports healthy while it cannot page is worse than one
// that is honestly down.
type Readiness interface {
	Ready(ctx context.Context) (ok bool, detail map[string]string)
}

type Server struct {
	manager   *call.Manager
	cfg       config.Config
	ready     Readiness
	log       *slog.Logger
	authToken []byte
	trunk     *trunk.Manager
	tts       *tts.Client
}

func NewServer(
	manager *call.Manager,
	cfg config.Config,
	ready Readiness,
	trunks *trunk.Manager,
	synthesizer *tts.Client,
	log *slog.Logger,
) *Server {
	return &Server{
		manager:   manager,
		cfg:       cfg,
		ready:     ready,
		log:       log,
		authToken: []byte(cfg.APIToken),
		trunk:     trunks,
		tts:       synthesizer,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// /calls is authenticated; /health is not, so an orchestrator can probe it.
	mux.Handle("POST /calls", s.authenticated(http.HandlerFunc(s.placeCall)))
	mux.HandleFunc("GET /health", s.health)
	// The carrier. This service holds none of its own; the caller puts one here.
	mux.Handle("PUT /trunk", s.authenticated(http.HandlerFunc(s.setTrunk)))
	mux.Handle("GET /trunk", s.authenticated(http.HandlerFunc(s.getTrunk)))

	// Renders text without placing a call, so what an announcement will say can be checked before
	// somebody's phone rings. Authenticated: synthesis is expensive and the text is incident text.
	mux.Handle("POST /preview", s.authenticated(http.HandlerFunc(s.preview)))
	mux.Handle("GET /preview/audio/{key}", s.authenticated(http.HandlerFunc(s.previewAudio)))
	return mux
}

// authenticated gates a handler behind the shared bearer token, compared in
// constant time so the check does not leak the token through timing.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(presented), s.authToken) != 1 {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type segment struct {
	Text string `json:"text"`
	Lang string `json:"lang"`
}

type responses struct {
	Acknowledged []segment `json:"acknowledged"`
	Escalated    []segment `json:"escalated"`
	Conference   []segment `json:"conference"`
	InvalidKey   []segment `json:"invalid_key"`
}

type keys struct {
	Acknowledge string `json:"acknowledge"`
	Escalate    string `json:"escalate"`
	Repeat      string `json:"repeat"`
	Conference  string `json:"conference"`
}

type limits struct {
	MaxDurationSeconds    int  `json:"max_duration_seconds"`
	SilenceTimeoutSeconds int  `json:"silence_timeout_seconds"`
	MaxReprompts          *int `json:"max_reprompts"`
}

type callRequest struct {
	CallID       string    `json:"call_id"`
	To           string    `json:"to"`
	Voice        string    `json:"voice"`
	Gate         []segment `json:"gate"`
	Announcement []segment `json:"announcement"`
	Prompt       []segment `json:"prompt"`
	Responses    responses `json:"responses"`
	Keys         keys      `json:"keys"`
	Limits       limits    `json:"limits"`
	CallbackURL  string    `json:"callback_url"`
}

func (s *Server) placeCall(w http.ResponseWriter, r *http.Request) {
	// Bound the body before reading it: an unbounded request is a trivial OOM.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req callRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}

	domain, err := s.toDomain(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	switch err := s.manager.Place(r.Context(), domain); {
	case err == nil:
	case errors.Is(err, tts.ErrRejected):
		// The caller's own text, voice or language. Answering 502 would tell them to retry
		// something that will be refused identically every time.
		fail(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, call.ErrDuplicate):
		// The caller retries pages; saying "already running" is safer than
		// dialling the same person twice.
		fail(w, http.StatusConflict, "call already in progress")
		return
	case errors.Is(err, call.ErrAtCapacity):
		fail(w, http.StatusServiceUnavailable, "at capacity")
		return
	case errors.Is(err, call.ErrDraining):
		// Same 503 — the caller must treat both as "refused, nothing was dialled" —
		// but the reason reaches an incident timeline, so it has to be true.
		fail(w, http.StatusServiceUnavailable, "shutting down")
		return
	default:
		// The underlying error can carry the endpoint (and thus the number); mask
		// it before it reaches the logs.
		s.log.Error("place call failed", "call_id", domain.CallID,
			"error", strings.ReplaceAll(err.Error(), domain.To, maskNumber(domain.To)))
		fail(w, http.StatusBadGateway, "failed to place call")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"call_id": domain.CallID,
		"status":  "placing",
	})
}

func (s *Server) toDomain(req callRequest) (voice.Request, error) {
	if req.CallID == "" {
		return voice.Request{}, errors.New("call_id is required")
	}
	if err := validateDestination(req.To, s.cfg.AllowedPrefixes); err != nil {
		return voice.Request{}, err
	}
	if err := validateCallbackURL(req.CallbackURL); err != nil {
		return voice.Request{}, err
	}

	k := voice.Keys{
		Acknowledge: or(req.Keys.Acknowledge, "1"),
		Escalate:    or(req.Keys.Escalate, "2"),
		Repeat:      or(req.Keys.Repeat, "*"),
		Conference:  or(req.Keys.Conference, "9"),
	}
	if err := validateKeys(k.Acknowledge, k.Escalate, k.Repeat, k.Conference); err != nil {
		return voice.Request{}, err
	}

	// An empty lang is rejected by the synthesizer, so it never reaches it.
	lang := s.cfg.DefaultLang

	out := voice.Request{
		CallID:       req.CallID,
		To:           req.To,
		Voice:        or(req.Voice, s.cfg.DefaultVoice),
		Gate:         segments(req.Gate, lang),
		Announcement: segments(req.Announcement, lang),
		Prompt:       segments(req.Prompt, lang),
		Responses: voice.Responses{
			Acknowledged: segments(req.Responses.Acknowledged, lang),
			Escalated:    segments(req.Responses.Escalated, lang),
			Conference:   segments(req.Responses.Conference, lang),
			InvalidKey:   segments(req.Responses.InvalidKey, lang),
		},
		Keys: k,
		Limits: voice.Limits{
			// Clamp to configured ceilings: a request must not be able to hold a
			// channel open for years by asking for a huge max_duration.
			MaxDuration:    clamp(req.Limits.MaxDurationSeconds, s.cfg.MaxDuration),
			SilenceTimeout: clamp(req.Limits.SilenceTimeoutSeconds, s.cfg.SilenceTimeout),
			MaxReprompts:   s.cfg.MaxReprompts,
		},
		CallbackURL: req.CallbackURL,
	}
	// A zero value is meaningful here (never reprompt), so only a missing field
	// falls back to the default; a positive value is capped.
	if req.Limits.MaxReprompts != nil {
		out.Limits.MaxReprompts = min(*req.Limits.MaxReprompts, s.cfg.MaxReprompts)
	}
	// A prompt that never ends because silence outlives the call makes no sense.
	if out.Limits.SilenceTimeout > out.Limits.MaxDuration {
		out.Limits.SilenceTimeout = out.Limits.MaxDuration
	}

	// Checked after clipping, not before: what matters is whether anything is left to say.
	if len(out.Announcement) == 0 {
		return voice.Request{}, errors.New("announcement is required")
	}
	if len(out.Prompt) == 0 {
		return voice.Request{}, errors.New("prompt is required")
	}
	// Refused rather than clipped: dropping segments would silently remove the menu or a confirmation.
	if total := out.SegmentCount(); total > tts.MaxSegments {
		return voice.Request{}, fmt.Errorf("a call may carry at most %d segments in total, got %d",
			tts.MaxSegments, total)
	}
	return out, nil
}

// validateCallbackURL refuses a URL the callback could never reach. Discovering it after the page
// has gone out means the responder's keypress lands nowhere.
func validateCallbackURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("callback_url is not a URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("callback_url must be http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("callback_url has no host")
	}
	return nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	ok, detail := s.ready.Ready(ctx)
	body := map[string]any{
		"status":       map[bool]string{true: "ok", false: "unavailable"}[ok],
		"active_calls": s.manager.Active(),
		"checks":       detail,
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(body)
}

// segments trims, drops what is left empty, and clips what is too long — clipped rather than
// refused, because losing the tail of a description is cheaper than losing the page.
func segments(in []segment, defaultLang string) []voice.Segment {
	out := make([]voice.Segment, 0, len(in))
	for _, s := range in {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		if len(text) > tts.MaxSegmentChars {
			text = clipRunes(text, tts.MaxSegmentChars)
		}
		out = append(out, voice.Segment{Text: text, Lang: or(s.Lang, defaultLang)})
	}
	return out
}

// clipRunes cuts to max bytes without splitting a rune, so a clipped Turkish description is still
// valid UTF-8 rather than a replacement character the synthesizer has to read out.
func clipRunes(text string, max int) string {
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// clamp turns a requested seconds value into a duration bounded by max. A
// missing (<=0) value takes the default; anything larger is capped at it.
func clamp(v int, max time.Duration) time.Duration {
	if v <= 0 {
		return max
	}
	if d := time.Duration(v) * time.Second; d < max {
		return d
	}
	return max
}

// maskNumber keeps the country prefix and last two digits, hiding the rest:
// +905321234567 -> +90******67.
func maskNumber(to string) string {
	if len(to) <= 5 {
		return "***"
	}
	return to[:3] + strings.Repeat("*", len(to)-5) + to[len(to)-2:]
}

// setTrunk replaces the carrier and reloads Asterisk. Calls already up are not disturbed;
// the module reload only changes what the next one dials through.
func (s *Server) setTrunk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)

	var settings trunk.Settings
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	state, err := s.trunk.Apply(r.Context(), settings)
	if err != nil {
		// A rejected carrier is the caller's mistake; a failed reload is ours.
		if verr := settings.Validate(); verr != nil {
			fail(w, http.StatusBadRequest, verr.Error())
			return
		}
		s.log.Error("applying trunk configuration", "error", err)
		fail(w, http.StatusInternalServerError, "could not apply the trunk configuration")
		return
	}

	s.log.Info("trunk configuration applied",
		"enabled", state.Enabled, "host", state.Host, "register", state.Register)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// getTrunk reports the carrier this process last applied, never its password. Its fingerprint
// is what the caller compares against its own record to decide whether to send one again.
func (s *Server) getTrunk(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.trunk.Current())
}

func fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// previewRequest is the free-text form of an announcement: segments the caller wants to hear, each
// with its own language, exactly as a real call carries them.
type previewRequest struct {
	Segments []voice.Segment `json:"segments"`
	Voice    string          `json:"voice"`
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)

	var body previewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	if len(body.Segments) == 0 {
		fail(w, http.StatusBadRequest, "at least one segment is required")
		return
	}
	if len(body.Segments) > tts.MaxSegments {
		fail(w, http.StatusBadRequest, fmt.Sprintf("at most %d segments", tts.MaxSegments))
		return
	}
	for i, seg := range body.Segments {
		if strings.TrimSpace(seg.Text) == "" {
			fail(w, http.StatusBadRequest, fmt.Sprintf("segment %d has no text", i))
			return
		}
		if utf8.RuneCountInString(seg.Text) > tts.MaxSegmentChars {
			fail(w, http.StatusBadRequest, fmt.Sprintf("segment %d is longer than %d characters", i, tts.MaxSegmentChars))
			return
		}
	}

	voiceName := body.Voice
	if voiceName == "" {
		voiceName = s.cfg.DefaultVoice
	}

	segments, err := s.tts.Preview(r.Context(), body.Segments, voiceName)
	if err != nil {
		// The synthesizer refusing the text, voice or language is the caller's to fix; anything else is ours.
		if errors.Is(err, tts.ErrRejected) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		// A page is going out on the same synthesizer. Saying so beats making it wait.
		if errors.Is(err, tts.ErrBusy) {
			w.Header().Set("Retry-After", "5")
			fail(w, http.StatusServiceUnavailable, "a call is being rendered right now; try the preview again shortly")
			return
		}
		s.log.Error("rendering preview", "error", err)
		fail(w, http.StatusBadGateway, "the synthesizer could not render this text")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"segments": segments})
}

func (s *Server) previewAudio(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	audio, err := s.tts.Audio(r.Context(), key)
	if err != nil {
		if errors.Is(err, tts.ErrRejected) {
			fail(w, http.StatusNotFound, "no rendered audio under that key")
			return
		}
		s.log.Error("reading preview audio", "error", err, "key", key)
		fail(w, http.StatusBadGateway, "the synthesizer could not return the audio")
		return
	}
	defer audio.Close()

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, audio); err != nil {
		s.log.Warn("streaming preview audio", "error", err, "key", key)
	}
}
