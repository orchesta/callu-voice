package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Shutdown sizes its drain from Budget. If Budget under-reports, a terminal status is killed
// mid-retry and the incident stays at "connected" forever.
func TestBudgetCoversEveryAttemptAndTheWaitsBetween(t *testing.T) {
	n := New(10*time.Second, 3, 2*time.Second, true, testLogger())

	// 3 attempts timing out, plus the 2s and 4s waited between them.
	if got, want := n.Budget(), 36*time.Second; got != want {
		t.Fatalf("Budget() = %v, want %v", got, want)
	}
}

func TestBudgetOfASingleAttemptIsJustItsTimeout(t *testing.T) {
	n := New(7*time.Second, 1, 2*time.Second, true, testLogger())

	if got, want := n.Budget(), 7*time.Second; got != want {
		t.Fatalf("Budget() = %v, want %v", got, want)
	}
}

// httptest binds loopback, which the dial guard blocks on purpose, so the retry loop is exercised
// through a client without it. The guard has its own test below; reaching a real server through
// the shipped one is not possible, and a test that "passed" against it would only be proving the
// guard fired.
func reachable(timeout time.Duration, attempts int, backoff time.Duration) *Notifier {
	return &Notifier{
		http:     &http.Client{Timeout: timeout},
		attempts: attempts,
		backoff:  backoff,
		log:      testLogger(),
	}
}

// The number Budget promises has to be the number Notify actually spends against an endpoint that
// never answers, or the drain is sized off a fiction.
func TestBudgetBoundsWhatNotifyActuallySpends(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-blocked
	}))
	defer server.Close()
	defer close(blocked)

	n := reachable(60*time.Millisecond, 3, 20*time.Millisecond)

	started := time.Now()
	err := n.Notify(context.Background(), server.URL, voice.Event{CallID: "c", Status: voice.StatusAcknowledged})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("an endpoint that never answers must be reported as a failure")
	}
	// Under, or the loop gave up early; far over, and the drain sized from Budget is too short.
	if elapsed < n.Budget()/2 {
		t.Fatalf("Notify gave up after %v, well short of the %v ladder it claims", elapsed, n.Budget())
	}
	if elapsed > n.Budget()*2 {
		t.Fatalf("Notify took %v, more than twice the %v it budgets for", elapsed, n.Budget())
	}
}

func TestNotifyStopsRetryingOnceItSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	n := reachable(time.Second, 3, 10*time.Millisecond)

	if err := n.Notify(context.Background(), server.URL, voice.Event{CallID: "c"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("a delivered callback was sent %d times; the receiver would see duplicates", got)
	}
}

func TestNotifyRetriesANon2xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	n := reachable(time.Second, 3, time.Millisecond)

	if err := n.Notify(context.Background(), server.URL, voice.Event{CallID: "c"}); err == nil {
		t.Fatal("a callback the receiver rejected was reported as delivered")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("a rejected callback was attempted %d times, want 3", got)
	}
}

// The callback carries the outcome the caller acts on, so an empty URL must be a silent no-op
// rather than an error that gets logged as a lost acknowledgement.
func TestNoCallbackURLIsNotAFailure(t *testing.T) {
	n := New(time.Second, 3, time.Millisecond, true, testLogger())

	if err := n.Notify(context.Background(), "", voice.Event{CallID: "c"}); err != nil {
		t.Fatalf("an absent callback URL reported an error: %v", err)
	}
}

func TestLoopbackIsBlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	// allowPrivate is on, which is the shipped default; loopback is blocked regardless of it.
	n := New(time.Second, 1, time.Millisecond, true, testLogger())

	if err := n.Notify(context.Background(), server.URL, voice.Event{CallID: "c"}); err == nil {
		t.Fatal("a callback to loopback was delivered; the dial guard is not applied")
	}
}
