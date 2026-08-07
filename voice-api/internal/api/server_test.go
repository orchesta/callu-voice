package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/orchesta/callu-voice/internal/config"
	"github.com/orchesta/callu-voice/internal/trunk"
	"github.com/orchesta/callu-voice/internal/tts"
	"github.com/orchesta/callu-voice/internal/voice"
)

// testConfig boots the real configuration, rather than assembling a Config
// literal, so this test sees whichever default the fix chooses: a hard-coded
// fallback in segments() and a new DEFAULT_LANG environment knob both satisfy it.
func testConfig(t *testing.T) config.Config {
	t.Helper()

	// The minimum Load() insists on: a non-placeholder API token of 16+ chars and
	// an ARI credential of 12+.
	t.Setenv("API_TOKEN", "0123456789abcdef0123")
	t.Setenv("ARI_USERNAME", "callu")
	t.Setenv("ARI_PASSWORD", "0123456789abcd")
	t.Setenv("ALLOWED_PREFIXES", "+90")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// An omitted segment lang must fall back to a default: the synthesizer treats "" as an
// unsupported language and refuses the whole render, so the page is never placed.
func TestOmittedSegmentLangFallsBackToADefault(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	// lang omitted everywhere — what a caller sends when its templates carry no
	// language column.
	req := callRequest{
		CallID:       "8f3c",
		To:           "+905321234567",
		Announcement: []segment{{Text: "payment-api servisinden acil seviyeli bildirim."}},
		Prompt:       []segment{{Text: "Onaylamak için bire basın."}},
		Responses: responses{
			Acknowledged: []segment{{Text: "Anlaşıldı. Olay onaylandı."}},
		},
	}

	domain, err := srv.toDomain(req)
	if err != nil {
		t.Fatalf("toDomain rejected a request with no lang: %v", err)
	}

	groups := []struct {
		name string
		segs []voice.Segment
	}{
		{"announcement", domain.Announcement},
		{"prompt", domain.Prompt},
		{"responses.acknowledged", domain.Responses.Acknowledged},
	}
	for _, g := range groups {
		for i, s := range g.segs {
			if s.Lang == "" {
				t.Errorf("%s[%d] carries an empty lang; the synthesizer treats \"\" as an "+
					"unsupported language, so the page is dropped instead of being spoken "+
					"in the default language", g.name, i)
			}
		}
	}
}

func TestAnExplicitSegmentLangSurvivesTheFallback(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := callRequest{
		CallID:       "8f3c",
		To:           "+905321234567",
		Announcement: []segment{{Text: "payment-api is down.", Lang: "en-US"}},
		Prompt:       []segment{{Text: "Press one to acknowledge.", Lang: "en-US"}},
	}

	domain, err := srv.toDomain(req)
	if err != nil {
		t.Fatalf("toDomain: %v", err)
	}
	if got := domain.Announcement[0].Lang; got != "en-US" {
		t.Errorf("announcement lang = %q, want en-US", got)
	}
	if got := domain.Prompt[0].Lang; got != "en-US" {
		t.Errorf("prompt lang = %q, want en-US", got)
	}
}

func validRequest() callRequest {
	return callRequest{
		CallID:       "8f3c",
		To:           "+905321234567",
		Announcement: []segment{{Text: "payment-api is down.", Lang: "en-US"}},
		Prompt:       []segment{{Text: "Press one to acknowledge.", Lang: "en-US"}},
	}
}

// An incident description is not something the caller composed, so a long one has to be clipped
// rather than refused: refusing it means the phone never rings at all.
func TestAnOverlongSegmentIsClippedRatherThanRefused(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := validRequest()
	req.Announcement = []segment{{Text: strings.Repeat("a", tts.MaxSegmentChars+500), Lang: "en-US"}}

	domain, err := srv.toDomain(req)
	if err != nil {
		t.Fatalf("an over-long description was refused instead of clipped: %v", err)
	}
	if got := len(domain.Announcement[0].Text); got != tts.MaxSegmentChars {
		t.Fatalf("clipped to %d chars, want %d", got, tts.MaxSegmentChars)
	}
}

// Clipping must not split a multi-byte character: the synthesizer would be handed invalid UTF-8.
func TestClippingKeepsTheTextValidUtf8(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := validRequest()
	// "ı" is two bytes, so a cut at the cap lands mid-rune for some repetitions.
	req.Announcement = []segment{{Text: strings.Repeat("ı", tts.MaxSegmentChars), Lang: "tr"}}

	domain, err := srv.toDomain(req)
	if err != nil {
		t.Fatalf("toDomain: %v", err)
	}
	if !utf8.ValidString(domain.Announcement[0].Text) {
		t.Fatal("clipping produced invalid UTF-8")
	}
}

func TestBlankSegmentsAreDropped(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := validRequest()
	req.Announcement = []segment{
		{Text: "  ", Lang: "en-US"},
		{Text: "payment-api is down.", Lang: "en-US"},
	}

	domain, err := srv.toDomain(req)
	if err != nil {
		t.Fatalf("toDomain: %v", err)
	}
	if len(domain.Announcement) != 1 {
		t.Fatalf("blank segment survived: %#v", domain.Announcement)
	}
}

// Nothing left to say is a caller bug that clipping cannot fix.
func TestAnAnnouncementThatIsOnlyWhitespaceIsRefused(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := validRequest()
	req.Announcement = []segment{{Text: "   ", Lang: "en-US"}}

	if _, err := srv.toDomain(req); err == nil {
		t.Fatal("a request with nothing to say was accepted; the callee would hear only the menu")
	}
}

func TestTooManySegmentsIsRefusedWithTheCount(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := validRequest()
	req.Announcement = make([]segment, tts.MaxSegments+1)
	for i := range req.Announcement {
		req.Announcement[i] = segment{Text: "line", Lang: "en-US"}
	}

	_, err := srv.toDomain(req)
	if err == nil {
		t.Fatal("a request past the synthesizer's segment cap was accepted; it would surface as a 502")
	}
	if !strings.Contains(err.Error(), "segments") {
		t.Errorf("the error does not name the problem: %v", err)
	}
}

// The segment cap covers every group, because they are rendered in one request.
func TestTheSegmentCapCountsEveryGroup(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	req := validRequest()
	fill := func(n int) []segment {
		out := make([]segment, n)
		for i := range out {
			out[i] = segment{Text: "line", Lang: "en-US"}
		}
		return out
	}
	req.Gate = fill(10)
	req.Announcement = fill(10)
	req.Prompt = fill(10)
	req.Responses.Acknowledged = fill(10)

	if _, err := srv.toDomain(req); err == nil {
		t.Fatal("40 segments spread across four groups passed a cap of 32")
	}
}

func TestCallbackURLMustBeUsable(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	for _, bad := range []string{
		"callu.example.com/api/voice/callback", // no scheme: fails only after the page went out
		"file:///tmp/x",
		"ftp://callu.example.com/hook",
		"https://",
	} {
		req := validRequest()
		req.CallbackURL = bad

		if _, err := srv.toDomain(req); err == nil {
			t.Errorf("callback_url %q was accepted; the responder's keypress would land nowhere", bad)
		}
	}
}

func TestAUsableCallbackURLIsAccepted(t *testing.T) {
	srv := &Server{cfg: testConfig(t)}

	for _, good := range []string{
		"",
		"http://callu.example.com/api/voice/callback",
		"https://callu.example.com:8443/api/voice/callback",
	} {
		req := validRequest()
		req.CallbackURL = good

		if _, err := srv.toDomain(req); err != nil {
			t.Errorf("callback_url %q was refused: %v", good, err)
		}
	}
}

func TestSegmentCountCoversEveryGroup(t *testing.T) {
	r := voice.Request{
		Gate:         make([]voice.Segment, 1),
		Announcement: make([]voice.Segment, 2),
		Prompt:       make([]voice.Segment, 3),
		Responses: voice.Responses{
			Acknowledged: make([]voice.Segment, 4),
			Escalated:    make([]voice.Segment, 5),
			Conference:   make([]voice.Segment, 6),
			InvalidKey:   make([]voice.Segment, 7),
		},
	}

	if got, want := r.SegmentCount(), 28; got != want {
		t.Fatalf("SegmentCount() = %d, want %d", got, want)
	}
}

// The carrier and its password are as sensitive as the call API itself: an unauthenticated PUT
// would let anyone on the network route every page through a carrier of their choosing.
func TestTheTrunkRoutesRefuseAnUnauthenticatedCaller(t *testing.T) {
	cfg := testConfig(t)
	srv := &Server{
		cfg:       cfg,
		log:       slog.New(slog.DiscardHandler),
		authToken: []byte(cfg.APIToken),
		trunk:     trunk.NewManager(filepath.Join(t.TempDir(), "pjsip_trunk.conf")),
	}
	routes := srv.Routes()

	for _, probe := range []struct {
		method string
		body   string
	}{
		{http.MethodPut, `{"enabled":false}`},
		{http.MethodGet, ""},
	} {
		request := httptest.NewRequest(probe.method, "/trunk", strings.NewReader(probe.body))
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s /trunk without a token answered %d, want 401", probe.method, recorder.Code)
		}
	}
}

// GET /trunk is the only way to ask what carrier is live, so it must not answer with the password.
func TestGetTrunkNeverAnswersWithThePassword(t *testing.T) {
	cfg := testConfig(t)
	srv := &Server{
		cfg:       cfg,
		log:       slog.New(slog.DiscardHandler),
		authToken: []byte(cfg.APIToken),
		trunk:     trunk.NewManager(filepath.Join(t.TempDir(), "pjsip_trunk.conf")),
	}

	request := httptest.NewRequest(http.MethodGet, "/trunk", nil)
	request.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	recorder := httptest.NewRecorder()
	srv.Routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /trunk answered %d, want 200", recorder.Code)
	}
	if strings.Contains(strings.ToLower(recorder.Body.String()), "\"password\"") {
		t.Fatalf("GET /trunk carries a password field: %s", recorder.Body.String())
	}
}

// The caller reconciles against this field, and without it the only way to tell whether the
// carrier is still there is to push one every time and hope.
func TestGetTrunkReportsAFingerprintTheCallerCanCompare(t *testing.T) {
	cfg := testConfig(t)
	srv := &Server{
		cfg:       cfg,
		log:       slog.New(slog.DiscardHandler),
		authToken: []byte(cfg.APIToken),
		trunk:     trunk.NewManager(filepath.Join(t.TempDir(), "pjsip_trunk.conf")),
	}

	request := httptest.NewRequest(http.MethodGet, "/trunk", nil)
	request.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	recorder := httptest.NewRecorder()
	srv.Routes().ServeHTTP(recorder, request)

	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Fingerprint != (trunk.Settings{}).Fingerprint() {
		t.Fatalf("a service holding no carrier does not say so by fingerprint: %s", recorder.Body.String())
	}
}
