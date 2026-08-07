package call

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

// blockingSynth holds Render until released, so a test can stand a call up mid-render:
// audio not finished, no flow registered yet.
type blockingSynth struct {
	entered chan struct{}
	release chan struct{}

	mu     sync.Mutex
	closed bool
}

func newBlockingSynth() *blockingSynth {
	return &blockingSynth{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *blockingSynth) Render(_ context.Context, segments []voice.Segment, _ string) ([]string, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release

	out := make([]string, len(segments))
	for i := range out {
		out[i] = "sound:callu/x"
	}
	return out, nil
}

func (s *blockingSynth) unblock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.release)
	}
}

type countingPBX struct {
	mu         sync.Mutex
	originated int
	hungUp     []voice.ChannelID
}

func (p *countingPBX) Originate(_ context.Context, _, _ string) (voice.ChannelID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.originated++
	return "chan-1", nil
}

func (p *countingPBX) Play(_ context.Context, _ voice.ChannelID, _ ...string) (voice.PlaybackID, error) {
	return "pb-1", nil
}

func (p *countingPBX) Stop(_ context.Context, _ voice.PlaybackID) error { return nil }

func (p *countingPBX) Hangup(_ context.Context, ch voice.ChannelID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hungUp = append(p.hungUp, ch)
	return nil
}

func (p *countingPBX) Variable(_ context.Context, _ voice.ChannelID, _ string) (string, error) {
	return "", nil
}

func (p *countingPBX) dialled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.originated
}

func (p *countingPBX) hangups() []voice.ChannelID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]voice.ChannelID(nil), p.hungUp...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func pageRequest(callID string) voice.Request {
	return voice.Request{
		CallID:       callID,
		To:           "+905551112233",
		Announcement: []voice.Segment{{Text: "payment-api down", Lang: "en-US"}},
		Prompt:       []voice.Segment{{Text: "press 1", Lang: "en-US"}},
	}
}

// A call caught mid-render when shutdown begins must never be dialled: it would ring
// a phone the process is about to abandon.
func TestPlaceDoesNotDialAfterDrainingBegins(t *testing.T) {
	synth := newBlockingSynth()
	pbx := &countingPBX{}
	m := NewManager(pbx, synth, &fakeNotifier{}, 0, quietLogger())

	placed := make(chan error, 1)
	go func() { placed <- m.Place(context.Background(), pageRequest("call-1")) }()

	select {
	case <-synth.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("render never started")
	}

	drained := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m.Drain(ctx)
		close(drained)
	}()

	// Drain must not be waiting on a call that can no longer be dialled.
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain waited for a call that was still rendering")
	}

	synth.unblock()

	select {
	case err := <-placed:
		if !errors.Is(err, ErrDraining) {
			t.Fatalf("want ErrDraining, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Place never returned")
	}

	if got := pbx.dialled(); got != 0 {
		t.Fatalf("a phone was dialled during shutdown: %d originate(s)", got)
	}
	if got := m.Active(); got != 0 {
		t.Fatalf("Active() = %d after the call was rejected", got)
	}
}

func TestPlaceRejectsWithDrainingOnceShutdownStarted(t *testing.T) {
	m := NewManager(&countingPBX{}, newAlwaysSynth(), &fakeNotifier{}, 0, quietLogger())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Drain(ctx)

	err := m.Place(context.Background(), pageRequest("call-2"))

	if !errors.Is(err, ErrDraining) {
		t.Fatalf("want ErrDraining, got %v", err)
	}
	if errors.Is(err, ErrAtCapacity) {
		t.Fatal("draining must not be reported as capacity")
	}
}

func TestDispatchHangsUpAnOrphanedChannel(t *testing.T) {
	pbx := &countingPBX{}
	m := NewManager(pbx, newAlwaysSynth(), &fakeNotifier{}, 0, quietLogger())

	m.Dispatch(voice.TelephonyEvent{
		Kind:      voice.EventAnswered,
		CallID:    "",
		ChannelID: "orphan-1",
	})

	deadline := time.After(2 * time.Second)
	for {
		if len(pbx.hangups()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("orphaned channel was never hung up")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := pbx.hangups()[0]; got != "orphan-1" {
		t.Fatalf("hung up %q, want orphan-1", got)
	}
}

// Widening the orphan hangup beyond EventAnswered would cut live calls.
func TestDispatchIgnoresLateEventsForFinishedCalls(t *testing.T) {
	pbx := &countingPBX{}
	m := NewManager(pbx, newAlwaysSynth(), &fakeNotifier{}, 0, quietLogger())

	for _, kind := range []voice.TelephonyEventKind{
		voice.EventRinging, voice.EventDTMF, voice.EventPlaybackFinished, voice.EventHungUp,
	} {
		m.Dispatch(voice.TelephonyEvent{Kind: kind, CallID: "gone", ChannelID: "chan-9"})
	}

	time.Sleep(100 * time.Millisecond)

	if got := pbx.hangups(); len(got) != 0 {
		t.Fatalf("hung up %v on late events", got)
	}
}

type alwaysSynth struct{}

func newAlwaysSynth() *alwaysSynth { return &alwaysSynth{} }

func (a *alwaysSynth) Render(_ context.Context, segments []voice.Segment, _ string) ([]string, error) {
	out := make([]string, len(segments))
	for i := range out {
		out[i] = "sound:callu/x"
	}
	return out, nil
}
