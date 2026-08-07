package call

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

// The tests below drive the whole call flow with no Asterisk and no synthesizer.
// That is the point of the ports in package voice: if the flow can be exercised
// with fakes, the seams are real.

type fakePBX struct {
	mu       sync.Mutex
	played   [][]string
	stopped  int
	hungUp   bool
	variable map[string]string
	err      error

	// Every playback gets its own id, as Asterisk gives them. A fake that
	// returns a constant would hide the bug these tests exist to prevent.
	nextID   int
	lastPlay voice.PlaybackID
}

func newFakePBX() *fakePBX {
	return &fakePBX{variable: map[string]string{}}
}

func (f *fakePBX) Originate(_ context.Context, _, _ string) (voice.ChannelID, error) {
	return "chan-1", f.err
}

func (f *fakePBX) Play(_ context.Context, _ voice.ChannelID, media ...string) (voice.PlaybackID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = append(f.played, media)
	f.nextID++
	f.lastPlay = voice.PlaybackID(fmt.Sprintf("pb-%d", f.nextID))
	return f.lastPlay, nil
}

func (f *fakePBX) playback() voice.PlaybackID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPlay
}

func (f *fakePBX) Stop(_ context.Context, _ voice.PlaybackID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	return nil
}

func (f *fakePBX) Hangup(_ context.Context, _ voice.ChannelID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hungUp = true
	return nil
}

func (f *fakePBX) Variable(_ context.Context, _ voice.ChannelID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.variable[name], nil
}

func (f *fakePBX) plays() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.played))
	copy(out, f.played)
	return out
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []voice.Event
}

func (n *fakeNotifier) Notify(_ context.Context, _ string, e voice.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
	return nil
}

func (n *fakeNotifier) statuses() []voice.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]voice.Status, 0, len(n.events))
	for _, e := range n.events {
		out = append(out, e.Status)
	}
	return out
}

func testMedia() media {
	return media{
		announcement: []string{"sound:callu/announce"},
		prompt:       []string{"sound:callu/prompt"},
		acknowledged: []string{"sound:callu/ack"},
		escalated:    []string{"sound:callu/esc"},
		conference:   []string{"sound:callu/conf"},
		invalidKey:   []string{"sound:callu/invalid"},
	}
}

func testRequest() voice.Request {
	return voice.Request{
		CallID: "call-1",
		To:     "+900000000000",
		Keys:   voice.Keys{Acknowledge: "1", Escalate: "2", Repeat: "*", Conference: "9"},
		Limits: voice.Limits{
			MaxDuration:    2 * time.Second,
			SilenceTimeout: 50 * time.Millisecond,
			MaxReprompts:   2,
		},
		CallbackURL: "http://callback",
	}
}

// run starts a flow and feeds it events, returning once the flow is finished.
func run(t *testing.T, pbx *fakePBX, notifier *fakeNotifier, req voice.Request, feed func(f *Flow)) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flow := newFlow(req, pbx, notifier, testMedia(), log)

	done := make(chan struct{})
	go func() {
		flow.Run(context.Background())
		close(done)
	}()

	feed(flow)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flow did not finish")
	}
}

func answered(f *Flow) {
	f.events <- voice.TelephonyEvent{Kind: voice.EventAnswered, ChannelID: "chan-1"}
}

func TestAcknowledgeEndsTheCall(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	want := []voice.Status{voice.StatusAlerting, voice.StatusConnected, voice.StatusAcknowledged}
	assertStatuses(t, notifier.statuses(), want)

	if !pbx.hungUp {
		t.Error("call was not hung up")
	}
}

func TestKeypressBargesInOnTheAnnouncement(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	if pbx.stopped == 0 {
		t.Error("playback was not stopped: the callee kept being talked over")
	}
}

func TestEscalateIsReported(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "2"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	assertLast(t, notifier.statuses(), voice.StatusEscalated)
}

func TestConferenceIsReported(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "9"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	assertLast(t, notifier.statuses(), voice.StatusConferenceRequested)
}

func TestRepeatReplaysTheAnnouncement(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "*"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	plays := pbx.plays()
	if len(plays) < 2 {
		t.Fatalf("expected the announcement to be replayed, got %d plays", len(plays))
	}
	if plays[1][0] != "sound:callu/announce" {
		t.Errorf("repeat did not replay the announcement, played %v", plays[1])
	}
	assertLast(t, notifier.statuses(), voice.StatusAcknowledged)
}

func TestInvalidKeyRepromptsWithoutEndingTheCall(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "5"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	plays := pbx.plays()
	if len(plays) < 2 || plays[1][0] != "sound:callu/invalid" {
		t.Errorf("invalid key did not play the invalid prompt, plays=%v", plays)
	}
	assertLast(t, notifier.statuses(), voice.StatusAcknowledged)
}

func TestSilenceRepromptsThenGivesUp(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	req := testRequest()
	req.Limits.MaxReprompts = 2

	run(t, pbx, notifier, req, func(f *Flow) {
		answered(f)
		// Each finished playback arms the silence timer; nobody ever presses a key.
		// The id must be the one actually playing, or the flow rightly ignores it.
		for i := 0; i < 4; i++ {
			time.Sleep(40 * time.Millisecond)
			f.events <- voice.TelephonyEvent{
				Kind:     voice.EventPlaybackFinished,
				Playback: pbx.playback(),
			}
			time.Sleep(80 * time.Millisecond)
		}
	})

	assertLast(t, notifier.statuses(), voice.StatusSilenceTimeout)

	// Announcement, then one playback per reprompt.
	if got := len(pbx.plays()); got < 3 {
		t.Errorf("expected 2 reprompts after the announcement, got %d plays", got)
	}
}

func TestHangupBeforeAnyKeyIsNoAnswer(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		f.events <- voice.TelephonyEvent{Kind: voice.EventHungUp, Cause: "No user responding"}
	})

	assertLast(t, notifier.statuses(), voice.StatusNoAnswer)
}

func TestAnsweringMachineIsNotPaged(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}
	pbx.variable["AMDSTATUS"] = "MACHINE"

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
	})

	assertLast(t, notifier.statuses(), voice.StatusVoicemail)

	// Reading an alert to a voicemail box is worse than useless: the incident is
	// never acknowledged and the retry ladder is wasted.
	if len(pbx.plays()) != 0 {
		t.Errorf("the announcement was read to a machine: %v", pbx.plays())
	}
}

func TestHardCapEndsARunawayCall(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	req := testRequest()
	req.Limits.MaxDuration = 150 * time.Millisecond
	req.Limits.SilenceTimeout = time.Hour // silence must not be what ends it

	run(t, pbx, notifier, req, func(f *Flow) {
		answered(f)
	})

	assertLast(t, notifier.statuses(), voice.StatusTimeout)
	if !pbx.hungUp {
		t.Error("a call that hit the hard cap was left up")
	}
}

func TestTerminalStatusIsReportedOnce(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished}
		// A hangup event arrives after the acknowledgement, as it always does.
		f.events <- voice.TelephonyEvent{Kind: voice.EventHungUp}
	})

	statuses := notifier.statuses()
	terminal := 0
	for _, s := range statuses {
		if s != voice.StatusAlerting && s != voice.StatusConnected {
			terminal++
		}
	}
	if terminal != 1 {
		t.Errorf("expected exactly one terminal status, got %v", statuses)
	}
}

// A stopped playback completes too. If the flow waits on the wrong one, it hangs
// up while the confirmation is still playing and the callee hears nothing.
func TestConfirmationIsNotCutOffByTheStoppedAnnouncement(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flow := newFlow(testRequest(), pbx, notifier, testMedia(), log)

	done := make(chan struct{})
	go func() {
		flow.Run(context.Background())
		close(done)
	}()

	answered(flow)
	time.Sleep(50 * time.Millisecond)
	announcement := pbx.playback()

	flow.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
	time.Sleep(50 * time.Millisecond)

	// Asterisk completes the announcement we just stopped. The flow must keep
	// waiting for the acknowledgement it is now playing.
	flow.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: announcement}
	time.Sleep(50 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("call ended on the stopped announcement, cutting the confirmation off")
	default:
	}

	confirmation := pbx.playback()
	if confirmation == announcement {
		t.Fatal("the confirmation reused the announcement's playback id")
	}
	flow.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: confirmation}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flow did not finish after the confirmation played")
	}
	assertLast(t, notifier.statuses(), voice.StatusAcknowledged)
}

func played(pbx *fakePBX, uri string) bool {
	for _, p := range pbx.plays() {
		for _, m := range p {
			if m == uri {
				return true
			}
		}
	}
	return false
}

// The detail must not play until a human presses a key — any key.
func TestGateWithholdsDetailUntilKeypress(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}
	m := testMedia()
	m.gate = []string{"sound:callu/gate"}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flow := newFlow(testRequest(), pbx, notifier, m, log)
	done := make(chan struct{})
	go func() { flow.Run(context.Background()); close(done) }()

	answered(flow)
	time.Sleep(60 * time.Millisecond)

	plays := pbx.plays()
	if len(plays) == 0 || plays[0][0] != "sound:callu/gate" {
		t.Fatalf("first playback was not the gate: %v", plays)
	}
	if played(pbx, "sound:callu/announce") {
		t.Fatal("announcement played before any keypress; a voicemail machine would record it")
	}

	// A keypress reveals the detail — and must not be counted as an acknowledgement.
	flow.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "5"}
	time.Sleep(60 * time.Millisecond)
	if !played(pbx, "sound:callu/announce") {
		t.Fatal("announcement not played after the gate keypress")
	}
	for _, s := range notifier.statuses() {
		if s == voice.StatusAcknowledged {
			t.Fatal("gate keypress was counted as an acknowledgement")
		}
	}

	// Now acknowledge for real.
	flow.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
	flow.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flow did not finish")
	}
	assertLast(t, notifier.statuses(), voice.StatusAcknowledged)
}

func TestAbortEmitsTerminalCallbackAndHangsUp(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flow := newFlow(testRequest(), pbx, notifier, testMedia(), log)
	done := make(chan struct{})
	go func() { flow.Run(context.Background()); close(done) }()

	answered(flow)
	time.Sleep(50 * time.Millisecond)
	flow.Abort()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not end the flow")
	}
	assertLast(t, notifier.statuses(), voice.StatusFailed)
	if !pbx.hungUp {
		t.Error("aborted call was not hung up")
	}
}

func assertStatuses(t *testing.T, got, want []voice.Status) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
	}
}

func assertLast(t *testing.T, got []voice.Status, want voice.Status) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("no statuses reported, want %v", want)
	}
	if last := got[len(got)-1]; last != want {
		t.Fatalf("last status = %v, want %v (all: %v)", last, want, got)
	}
}

// stallingNotifier blocks Notify on chosen statuses, so a slow callback endpoint can
// be stood up in a test.
type stallingNotifier struct {
	mu       sync.Mutex
	events   []voice.Event
	release  chan struct{}
	stalling map[voice.Status]bool
}

func newStallingNotifier(stallOn ...voice.Status) *stallingNotifier {
	s := &stallingNotifier{release: make(chan struct{}), stalling: map[voice.Status]bool{}}
	for _, st := range stallOn {
		s.stalling[st] = true
	}
	return s
}

// Recorded AFTER the stall, so the slice is delivery order rather than call order.
func (s *stallingNotifier) Notify(_ context.Context, _ string, e voice.Event) error {
	s.mu.Lock()
	stall := s.stalling[e.Status]
	s.mu.Unlock()

	if stall {
		<-s.release
	}

	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	return nil
}

func (s *stallingNotifier) statuses() []voice.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]voice.Status, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Status)
	}
	return out
}

func TestASlowProgressCallbackDoesNotStallTheCall(t *testing.T) {
	notifier := newStallingNotifier(voice.StatusAlerting, voice.StatusConnected)
	pbx := newFakePBX()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flow := newFlow(testRequest(), pbx, notifier, testMedia(), log)

	done := make(chan struct{})
	go func() {
		flow.Run(context.Background())
		close(done)
	}()

	answered(flow)
	flow.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1", ChannelID: "chan-1"}

	// The announcement and the confirmation play while both progress callbacks are
	// still blocked.
	deadline := time.After(2 * time.Second)
	for {
		if len(pbx.plays()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("nothing was played while the progress callbacks were stalled")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(notifier.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flow did not finish")
	}

	got := notifier.statuses()
	if len(got) == 0 || got[len(got)-1] != voice.StatusAcknowledged {
		t.Fatalf("the terminal status must be delivered, and last: %v", got)
	}
}

// Within progressDrainGrace the terminal callback drains the queue ahead of it, so a
// caller never sees an ack for a call it thinks never connected.
func TestATerminalCallbackWaitsForQueuedProgress(t *testing.T) {
	notifier := newStallingNotifier(voice.StatusConnected)
	pbx := newFakePBX()

	req := testRequest()
	req.Limits.MaxDuration = 10 * time.Second // long enough that the cap is not what ends this

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	flow := newFlow(req, pbx, notifier, testMedia(), log)

	done := make(chan struct{})
	go func() {
		flow.Run(context.Background())
		close(done)
	}()

	answered(flow) // reports connected — queued, and the sender blocks on it
	flow.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1", ChannelID: "chan-1"}

	// Let the confirmation finish so the flow reaches its terminal report promptly,
	// with `connected` still in flight. Release only after that, inside the grace.
	go func() {
		time.Sleep(100 * time.Millisecond)
		flow.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
		time.Sleep(100 * time.Millisecond)
		close(notifier.release)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flow did not finish")
	}

	got := notifier.statuses()
	iConnected, iAck := -1, -1
	for i, s := range got {
		switch s {
		case voice.StatusConnected:
			iConnected = i
		case voice.StatusAcknowledged:
			iAck = i
		}
	}
	if iConnected == -1 || iAck == -1 || iConnected > iAck {
		t.Fatalf("connected must be delivered before acknowledged: %v", got)
	}
}

// The adapter drops these too; the flow guards it again because the cost of it slipping
// through is a closed incident nobody attended.
func TestKeypressBeforeTheAnswerDoesNotAcknowledge(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventHungUp, ChannelID: "chan-1"}
	})

	assertLast(t, notifier.statuses(), voice.StatusNoAnswer)

	for _, s := range notifier.statuses() {
		if s == voice.StatusAcknowledged {
			t.Fatal("a call nobody answered was reported as acknowledged")
		}
	}
}

// The same digit still works once the call is up, so the guard cannot be satisfied by refusing
// every keypress.
func TestKeypressAfterTheAnswerStillAcknowledges(t *testing.T) {
	pbx, notifier := newFakePBX(), &fakeNotifier{}

	run(t, pbx, notifier, testRequest(), func(f *Flow) {
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		answered(f)
		f.events <- voice.TelephonyEvent{Kind: voice.EventDTMF, Digit: "1"}
		f.events <- voice.TelephonyEvent{Kind: voice.EventPlaybackFinished, Playback: pbx.playback()}
	})

	assertLast(t, notifier.statuses(), voice.StatusAcknowledged)
}
