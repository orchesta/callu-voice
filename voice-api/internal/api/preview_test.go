package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchesta/callu-voice/internal/tts"
	"github.com/orchesta/callu-voice/internal/voice"
)

// A synthesizer that answers /synthesize the way the real one does, and records what it was asked.
func stubSynthesizer(t *testing.T, asked *map[string]any) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/synthesize":
			if asked != nil {
				_ = json.NewDecoder(r.Body).Decode(asked)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"results": [
					{"sound": "callu/0123456789abcdef0123456789abcdef", "cached": false,
					 "normalized": "Onaylamak için bire basın.", "duration_s": 2.5}
				],
				"media": ["sound:callu/0123456789abcdef0123456789abcdef"]
			}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/audio/"):
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFF....WAVE"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func previewServer(t *testing.T, synthURL string) *Server {
	t.Helper()
	cfg := testConfig(t)
	return &Server{
		cfg:       cfg,
		log:       slog.Default(),
		authToken: []byte(cfg.APIToken),
		tts:       tts.New(synthURL, 5*time.Second),
	}
}

// The normalized text is the whole point: it is what the synthesizer actually spoke, so a number
// left unread or a language that never applied is visible without anyone dialling a phone.
func TestPreviewReportsWhatEachSegmentBecame(t *testing.T) {
	var asked map[string]any
	synth := stubSynthesizer(t, &asked)
	defer synth.Close()

	srv := previewServer(t, synth.URL)

	body := strings.NewReader(`{"segments":[{"text":"Onaylamak için 1'e basın.","lang":"tr-TR"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/preview", body)
	rec := httptest.NewRecorder()

	srv.preview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Segments []tts.PreviewSegment `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding preview response: %v", err)
	}
	if len(out.Segments) != 1 {
		t.Fatalf("expected one segment, got %d", len(out.Segments))
	}
	if out.Segments[0].Normalized != "Onaylamak için bire basın." {
		t.Errorf("normalized text not carried back: %q", out.Segments[0].Normalized)
	}
	// The sound id is a path the caller should not have to know about; the key is what /preview/audio takes.
	if out.Segments[0].Key != "0123456789abcdef0123456789abcdef" {
		t.Errorf("key not stripped to the audio id: %q", out.Segments[0].Key)
	}
}

// The segment's own language has to reach the synthesizer, or the preview is rendered under some
// other language and says nothing useful about the call it stands in for.
func TestPreviewSendsEachSegmentsOwnLanguage(t *testing.T) {
	var asked map[string]any
	synth := stubSynthesizer(t, &asked)
	defer synth.Close()

	srv := previewServer(t, synth.URL)

	body := strings.NewReader(
		`{"segments":[{"text":"Checkout API servisinde","lang":"tr-TR"},{"text":"Search Cluster","lang":"en-US"}]}`)
	rec := httptest.NewRecorder()
	srv.preview(rec, httptest.NewRequest(http.MethodPost, "/preview", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", rec.Code, rec.Body.String())
	}

	segments, _ := asked["segments"].([]any)
	if len(segments) != 2 {
		t.Fatalf("the synthesizer was asked for %d segments", len(segments))
	}
	first, _ := segments[0].(map[string]any)
	second, _ := segments[1].(map[string]any)
	if first["lang"] != "tr-TR" || second["lang"] != "en-US" {
		t.Errorf("languages did not survive: %v / %v", first["lang"], second["lang"])
	}
}

func TestPreviewRefusesAnEmptyRequest(t *testing.T) {
	synth := stubSynthesizer(t, nil)
	defer synth.Close()

	srv := previewServer(t, synth.URL)

	for _, body := range []string{`{"segments":[]}`, `{"segments":[{"text":"   ","lang":"tr-TR"}]}`} {
		rec := httptest.NewRecorder()
		srv.preview(rec, httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(body)))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", body, rec.Code)
		}
	}
}

// Synthesis is expensive and the text is incident text; neither may be reachable unauthenticated.
func TestPreviewRoutesRequireAuthentication(t *testing.T) {
	synth := stubSynthesizer(t, nil)
	defer synth.Close()

	routes := previewServer(t, synth.URL).Routes()

	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(`{"segments":[{"text":"hi","lang":"en-US"}]}`)),
		httptest.NewRequest(http.MethodGet, "/preview/audio/0123456789abcdef0123456789abcdef", nil),
	} {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, r)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d, want 401", r.Method, r.URL.Path, rec.Code)
		}
	}
}

func TestPreviewAudioStreamsTheRenderedSegment(t *testing.T) {
	synth := stubSynthesizer(t, nil)
	defer synth.Close()

	srv := previewServer(t, synth.URL)

	req := httptest.NewRequest(http.MethodGet, "/preview/audio/0123456789abcdef0123456789abcdef", nil)
	req.SetPathValue("key", "0123456789abcdef0123456789abcdef")
	rec := httptest.NewRecorder()

	srv.previewAudio(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("previewAudio returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Errorf("content type %q", got)
	}
	if rec.Body.Len() == 0 {
		t.Error("no audio was streamed")
	}
}

// A preview shares one serialized synthesizer with the calls going out on it, so it is marked
// discretionary and turned away rather than made to queue in front of a phone that is ringing.
func TestPreviewIsMarkedDiscretionary(t *testing.T) {
	var asked map[string]any
	synth := stubSynthesizer(t, &asked)
	defer synth.Close()

	server := previewServer(t, synth.URL)

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"segments":[{"text":"Onaylamak için 1'e basın.","lang":"tr-TR"}]}`)
	server.preview(rec, httptest.NewRequest(http.MethodPost, "/preview", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("preview: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if asked["discretionary"] != true {
		t.Fatalf("preview did not mark itself discretionary: %v", asked["discretionary"])
	}
}

// A call render carries no such mark, or the synthesizer would turn away the page itself.
func TestACallRenderIsNotDiscretionary(t *testing.T) {
	var asked map[string]any
	synth := stubSynthesizer(t, &asked)
	defer synth.Close()

	client := tts.New(synth.URL, 5*time.Second)
	if _, err := client.Render(t.Context(), []voice.Segment{{Text: "Onaylamak için bire basın.", Lang: "tr-TR"}}, "F1"); err != nil {
		t.Fatalf("render: %v", err)
	}

	if _, marked := asked["discretionary"]; marked {
		t.Fatalf("a call render sent discretionary: %v", asked["discretionary"])
	}
}

// 503 from the synthesizer means the moment is wrong, not the text. Answered as such, with the
// Retry-After a caller needs, rather than as a 502 that reads like a fault.
func TestPreviewYieldsWhenACallIsBeingRendered(t *testing.T) {
	synth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"a call is being rendered; try again shortly"}`))
	}))
	defer synth.Close()

	server := previewServer(t, synth.URL)

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"segments":[{"text":"Onaylamak için 1'e basın.","lang":"tr-TR"}]}`)
	server.preview(rec, httptest.NewRequest(http.MethodPost, "/preview", body))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a 503 with no Retry-After leaves the caller guessing")
	}
	if !strings.Contains(rec.Body.String(), "call is being rendered") {
		t.Fatalf("the reason did not survive: %s", rec.Body.String())
	}
}

// The same 503 on a call render stays a fault: nothing there is discretionary, so a "come back
// later" would hide a synthesizer that cannot serve at all.
func TestACallRenderTreatsA503AsAFailure(t *testing.T) {
	synth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"model not ready"}`))
	}))
	defer synth.Close()

	client := tts.New(synth.URL, 5*time.Second)
	_, err := client.Render(t.Context(), []voice.Segment{{Text: "Onaylamak için bire basın.", Lang: "tr-TR"}}, "F1")

	if err == nil {
		t.Fatal("a call render accepted a 503")
	}
	if errors.Is(err, tts.ErrBusy) {
		t.Fatalf("a call render was told to come back later: %v", err)
	}
}
