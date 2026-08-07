// Package call holds the call flow: what the callee hears, what their keypresses
// mean, and when the call gives up. It talks to the PBX and the synthesizer only
// through the interfaces in package voice, so it can be driven without either.
package call

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

// media holds everything the flow may need to play, rendered once up front.
// Synthesis is far too slow to sit between a keypress and its confirmation.
type media struct {
	gate         []string
	announcement []string
	prompt       []string
	acknowledged []string
	escalated    []string
	conference   []string
	invalidKey   []string
}

// Flow drives a single call.
type Flow struct {
	req    voice.Request
	pbx    voice.Telephony
	notify voice.Notifier
	log    *slog.Logger

	media   media
	channel voice.ChannelID
	events  chan voice.TelephonyEvent

	// Closed by Abort; only the flow's own goroutine may touch report/settled, so it
	// is the one that emits the terminal callback.
	abort     chan struct{}
	abortOnce sync.Once

	// Progress (non-terminal) callbacks leave through here so a slow callback
	// endpoint cannot stall the event loop while a channel is up.
	progress     chan voice.Event
	progressDone chan struct{}
	progressShut sync.Once

	playback  voice.PlaybackID
	playing   bool
	gated     bool // a gate prompt is playing; the next keypress reveals the detail
	reprompts int
	answered  time.Time
	settled   bool // a terminal status has been reported
}

func newFlow(req voice.Request, pbx voice.Telephony, notify voice.Notifier, m media, log *slog.Logger) *Flow {
	return &Flow{
		req:    req,
		pbx:    pbx,
		notify: notify,
		media:  m,
		log:    log.With("call_id", req.CallID),
		// Buffered: the PBX event pump must never block on a slow flow.
		events: make(chan voice.TelephonyEvent, 16),
		abort:  make(chan struct{}),
		// Small on purpose: a call only reports alerting and connected before it
		// ends, so anything beyond that means the endpoint is not keeping up.
		progress:     make(chan voice.Event, 4),
		progressDone: make(chan struct{}),
	}
}

// Abort asks the flow to end promptly with a terminal status. Safe to call from
// another goroutine; the flow itself does the reporting.
func (f *Flow) Abort() {
	f.abortOnce.Do(func() { close(f.abort) })
}

// Run blocks until the call reaches a terminal state.
func (f *Flow) Run(ctx context.Context) {
	defer f.hangup(context.WithoutCancel(ctx))

	// Delivers progress callbacks in order, off the event loop. Detached from ctx:
	// its whole point is to outlive a torn-down call rather than be cancelled with it.
	go f.deliverProgress(context.WithoutCancel(ctx))
	defer f.closeProgress()

	// The hard cap starts at origination, not at answer: a call stuck ringing
	// forever still holds a trunk slot.
	ctx, cancel := context.WithTimeout(ctx, f.req.Limits.MaxDuration)
	defer cancel()

	// The call is on its way. Reported here rather than on a ringing event: the
	// PBX may never send one, and the caller only needs to know the page left.
	f.report(ctx, voice.StatusAlerting, nil)

	var silence <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			// Distinguish "we ran out of time" from "the service is shutting down".
			if ctx.Err() == context.DeadlineExceeded {
				f.report(context.WithoutCancel(ctx), voice.StatusTimeout, nil)
			}
			return

		case <-f.abort:
			// Graceful shutdown: report a terminal status so the incident does not stay at "connected".
			f.report(context.WithoutCancel(ctx), voice.StatusFailed, map[string]string{"reason": "shutdown"})
			return

		case <-silence:
			silence = nil
			if f.reprompts >= f.req.Limits.MaxReprompts {
				f.report(ctx, voice.StatusSilenceTimeout, map[string]string{
					"reprompts": itoa(f.reprompts),
				})
				return
			}
			f.reprompts++
			f.log.Info("no keypress; reprompting", "attempt", f.reprompts)
			f.play(ctx, f.media.prompt)

		case ev := <-f.events:
			switch ev.Kind {
			case voice.EventRinging:
				// Already reported as alerting when the call was placed.

			case voice.EventAnswered:
				f.channel = ev.ChannelID
				f.answered = time.Now()

				// Answering-machine detection runs in the dialplan before the
				// channel reaches us, so the verdict is already on the channel.
				if f.isMachine(ctx) {
					f.report(ctx, voice.StatusVoicemail, nil)
					return
				}

				f.report(ctx, voice.StatusConnected, nil)
				if len(f.media.gate) > 0 {
					// Play only the incident-free gate; the detail waits for a
					// human keypress so it never reaches a voicemail machine.
					f.gated = true
					f.play(ctx, f.media.gate)
				} else {
					f.play(ctx, f.announcementAndPrompt())
				}

			case voice.EventPlaybackFinished:
				// A playback stopped for barge-in completes too; that event is not this one.
				if ev.Playback != f.playback {
					continue
				}
				f.playing = false
				// Only start counting silence once the menu has actually been read.
				silence = time.After(f.req.Limits.SilenceTimeout)

			case voice.EventDTMF:
				// Deliberately duplicated from the adapter: an unanswered call must never acknowledge.
				if f.answered.IsZero() {
					f.log.Warn("ignoring a keypress before the call was answered", "digit", ev.Digit)
					continue
				}
				if done := f.onDigit(ctx, ev.Digit); done {
					return
				}
				// Any keypress resets the silence timer; the callee is clearly there.
				silence = nil

			case voice.EventHungUp:
				// Nobody pressed anything — never answered, or hung up mid-announcement.
				f.report(ctx, voice.StatusNoAnswer, causeData(ev.Cause))
				return
			}
		}
	}
}

// onDigit reports true when the call is over.
func (f *Flow) onDigit(ctx context.Context, digit string) bool {
	// Barge-in: stop talking the moment the callee acts.
	f.stopPlayback(ctx)

	// The first keypress while gated is proof of a human, not an action.
	if f.gated {
		f.gated = false
		f.log.Info("gate passed; playing detail")
		f.play(ctx, f.announcementAndPrompt())
		return false
	}

	switch digit {
	case f.req.Keys.Acknowledge:
		f.playAndWait(ctx, f.media.acknowledged)
		f.report(ctx, voice.StatusAcknowledged, dtmfData(digit))
		return true

	case f.req.Keys.Escalate:
		f.playAndWait(ctx, f.media.escalated)
		f.report(ctx, voice.StatusEscalated, dtmfData(digit))
		return true

	case f.req.Keys.Conference:
		f.playAndWait(ctx, f.media.conference)
		f.report(ctx, voice.StatusConferenceRequested, dtmfData(digit))
		return true

	case f.req.Keys.Repeat:
		f.log.Info("repeat requested")
		f.play(ctx, f.announcementAndPrompt())
		return false

	default:
		f.log.Info("invalid key", "digit", digit)
		f.play(ctx, append(append([]string{}, f.media.invalidKey...), f.media.prompt...))
		return false
	}
}

func (f *Flow) announcementAndPrompt() []string {
	out := make([]string, 0, len(f.media.announcement)+len(f.media.prompt))
	out = append(out, f.media.announcement...)
	out = append(out, f.media.prompt...)
	return out
}

func (f *Flow) play(ctx context.Context, m []string) {
	if len(m) == 0 || f.channel == "" {
		return
	}
	pb, err := f.pbx.Play(ctx, f.channel, m...)
	if err != nil {
		f.log.Error("play failed", "error", err)
		return
	}
	f.playback = pb
	f.playing = true
}

// playAndWait plays a short confirmation and gives it time to be heard before the
// call is torn down. Hanging up mid-word tells the callee nothing.
func (f *Flow) playAndWait(ctx context.Context, m []string) {
	if len(m) == 0 {
		return
	}
	f.play(ctx, m)
	waitFor := f.playback

	deadline := time.After(confirmationGrace)
	for {
		select {
		case ev := <-f.events:
			switch ev.Kind {
			case voice.EventPlaybackFinished:
				// The announcement we just stopped for barge-in completes too.
				// Waiting on the wrong one cuts the confirmation off mid-word.
				if ev.Playback == waitFor {
					return
				}
			case voice.EventHungUp:
				return
			}
		case <-deadline:
			return
		case <-f.abort:
			// Shutting down: the keypress is already recorded, so the confirmation can be cut.
			return
		case <-ctx.Done():
			return
		}
	}
}

func (f *Flow) stopPlayback(ctx context.Context) {
	if !f.playing || f.playback == "" {
		return
	}
	if err := f.pbx.Stop(ctx, f.playback); err != nil {
		f.log.Debug("stop playback failed", "error", err)
	}
	f.playing = false
}

func (f *Flow) isMachine(ctx context.Context) bool {
	status, err := f.pbx.Variable(ctx, f.channel, "AMDSTATUS")
	if err != nil {
		// Detection is optional; a missing verdict must not drop the page.
		return false
	}
	return status == "MACHINE"
}

func (f *Flow) hangup(ctx context.Context) {
	if f.channel == "" {
		return
	}
	if err := f.pbx.Hangup(ctx, f.channel); err != nil {
		f.log.Debug("hangup failed", "error", err)
	}
}

// report sends one event to the caller. Terminal statuses are reported once:
// a second "acknowledged" would let a stale event resurrect a closed incident.
func (f *Flow) report(ctx context.Context, status voice.Status, data map[string]string) {
	if f.settled {
		return
	}
	terminal := isTerminal(status)
	if terminal {
		f.settled = true
		// The terminal outcome must survive the teardown that cancelled ctx.
		ctx = context.WithoutCancel(ctx)
	}

	var duration float64
	if !f.answered.IsZero() {
		duration = time.Since(f.answered).Seconds()
	}

	event := voice.Event{
		CallID:    f.req.CallID,
		Status:    status,
		Duration:  duration,
		Data:      data,
		Timestamp: time.Now().UTC(),
	}

	f.log.Info("call status", "status", status)

	if !terminal {
		// Never blocks: a slow endpoint must not leave the callee hearing dead air.
		select {
		case f.progress <- event:
		default:
			f.log.Warn("dropping progress callback: queue full", "status", status)
		}
		return
	}

	// Terminal: delivered on this goroutine, after the queued progress callbacks, so
	// an acknowledgement cannot arrive before the connected that preceded it.
	f.closeProgress()

	if err := f.notify.Notify(ctx, f.req.CallbackURL, event); err != nil {
		f.log.Error("callback failed", "status", status, "error", err)
	}
}

func (f *Flow) deliverProgress(ctx context.Context) {
	defer close(f.progressDone)
	for event := range f.progress {
		if err := f.notify.Notify(ctx, f.req.CallbackURL, event); err != nil {
			f.log.Error("progress callback failed", "status", event.Status, "error", err)
		}
	}
}

// closeProgress stops the queue and waits, briefly, for what is already in it.
// Idempotent: both the terminal path and Run's defer call it.
func (f *Flow) closeProgress() {
	f.progressShut.Do(func() { close(f.progress) })

	select {
	case <-f.progressDone:
	case <-time.After(progressDrainGrace):
		f.log.Warn("progress callbacks still in flight; not waiting further")
	}
}

func isTerminal(s voice.Status) bool {
	switch s {
	case voice.StatusAlerting, voice.StatusConnected:
		return false
	default:
		return true
	}
}

func dtmfData(digit string) map[string]string {
	return map[string]string{"method": "dtmf", "digit": digit}
}

func causeData(cause string) map[string]string {
	if cause == "" {
		return nil
	}
	return map[string]string{"cause": cause}
}
