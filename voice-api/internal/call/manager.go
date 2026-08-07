package call

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

// confirmationGrace bounds how long a spoken confirmation may hold the call open
// after a keypress.
const confirmationGrace = 10 * time.Second

// progressDrainGrace bounds how long a terminal callback waits for the queued
// progress callbacks ahead of it. Short: ordering is worth a moment, not a call.
const progressDrainGrace = 2 * time.Second

// ErrDuplicate means a call with the same id is already running. Callers retry
// pages; without this a retry would dial the same person twice.
var ErrDuplicate = errors.New("call already in progress")

// ErrAtCapacity means the concurrent-call limit is reached. A caller that floods
// pages must be back-pressured, not allowed to exhaust the trunk and the box.
var ErrAtCapacity = errors.New("at capacity")

// ErrDraining means the process is shutting down. Distinct from ErrAtCapacity on purpose.
var ErrDraining = errors.New("shutting down")

// Manager owns the running calls and routes PBX events to them.
type Manager struct {
	pbx    voice.Telephony
	tts    voice.Synthesizer
	notify voice.Notifier
	log    *slog.Logger

	// A slot per in-flight call. Acquired before the expensive render so a flood
	// is rejected early; nil (no channel) means unlimited.
	sem chan struct{}

	// Set during shutdown so no new call is accepted while in-flight ones drain.
	draining atomic.Bool

	// Calls inside Place with no flow registered yet — rendering, mostly. Reported
	// by /health; drain deliberately does not wait on it (see activeFlows).
	placing atomic.Int64

	mu    sync.RWMutex
	flows map[string]*Flow // by call id
}

func NewManager(pbx voice.Telephony, tts voice.Synthesizer, notify voice.Notifier, maxConcurrent int, log *slog.Logger) *Manager {
	var sem chan struct{}
	if maxConcurrent > 0 {
		sem = make(chan struct{}, maxConcurrent)
	}
	return &Manager{
		pbx:    pbx,
		tts:    tts,
		notify: notify,
		log:    log,
		sem:    sem,
		flows:  make(map[string]*Flow),
	}
}

// Place renders the audio, dials, and returns as soon as the call is under way.
// Rendering happens before dialling on purpose: a synthesis failure should cancel
// the page, not leave someone listening to silence.
func (m *Manager) Place(ctx context.Context, req voice.Request) error {
	if m.draining.Load() {
		return ErrDraining
	}

	// So /health does not read zero while a page is being synthesized.
	m.placing.Add(1)
	defer m.placing.Add(-1)

	m.mu.RLock()
	_, exists := m.flows[req.CallID]
	m.mu.RUnlock()
	if exists {
		return ErrDuplicate
	}

	// Take a capacity slot before doing any expensive work. Released on every
	// exit path below, and by the flow goroutine on success.
	if !m.acquire() {
		return ErrAtCapacity
	}

	rendered, err := m.render(ctx, req)
	if err != nil {
		m.release()
		return fmt.Errorf("render: %w", err)
	}

	flow := newFlow(req, m.pbx, m.notify, rendered, m.log)

	m.mu.Lock()
	if _, exists := m.flows[req.CallID]; exists {
		m.mu.Unlock()
		m.release()
		return ErrDuplicate
	}
	m.flows[req.CallID] = flow
	m.mu.Unlock()

	// Re-checked after the render: synthesis takes seconds, and dialling someone
	// as the process exits wakes them for a call nothing will ever report on.
	if m.draining.Load() {
		m.forget(req.CallID)
		m.release()
		return ErrDraining
	}

	if _, err := m.pbx.Originate(ctx, req.CallID, req.To); err != nil {
		m.forget(req.CallID)
		m.release()
		// No callback: the caller has no record of this call yet. A stray channel is
		// cleaned up in ARI.Originate.
		return fmt.Errorf("originate: %w", err)
	}

	go func() {
		// Detached from the request context: the call outlives the HTTP request
		// that asked for it.
		flow.Run(context.WithoutCancel(ctx))
		m.forget(req.CallID)
		m.release()
	}()

	return nil
}

func (m *Manager) acquire() bool {
	if m.sem == nil {
		return true
	}
	select {
	case m.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *Manager) release() {
	if m.sem != nil {
		<-m.sem
	}
}

// Dispatch hands a PBX event to the flow that owns the channel. The telephony
// layer has already resolved which call the channel belongs to.
func (m *Manager) Dispatch(ev voice.TelephonyEvent) {
	m.mu.RLock()
	flow := m.flows[ev.CallID]
	m.mu.RUnlock()

	if flow == nil {
		// An answered channel with nobody to own it is an orphan of an originate whose
		// answer was lost; left alone the responder holds a silent, unlimited call.
		if ev.Kind == voice.EventAnswered {
			m.log.Warn("hanging up an orphaned channel", "channel", ev.ChannelID)
			go func(ch voice.ChannelID) {
				if err := m.pbx.Hangup(context.Background(), ch); err != nil {
					m.log.Warn("could not hang up orphaned channel", "channel", ch, "error", err)
				}
			}(ev.ChannelID)
		}
		// Everything else: events arriving after a flow has finished. Not an error.
		return
	}

	select {
	case flow.events <- ev:
	default:
		// A flow that cannot keep up must not stall the whole event pump.
		m.log.Warn("dropping event: flow queue full", "call_id", ev.CallID, "kind", ev.Kind)
	}
}

// Active reports calls in progress, including those still rendering. placing is
// read first so the count can only double-count, never miss a call.
func (m *Manager) Active() int {
	placing := int(m.placing.Load())
	return m.activeFlows() + placing
}

// activeFlows is what drain waits on, not Active(): a call still rendering can no
// longer be dialled once draining is set, so waiting on it only burns the grace period.
func (m *Manager) activeFlows() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.flows)
}

// Drain stops accepting new calls and asks every in-flight call to end with a
// terminal status, then waits for them to finish or the context to expire.
func (m *Manager) Drain(ctx context.Context) {
	m.draining.Store(true)

	m.abortAll() // the flow's own goroutine emits the terminal callback + hangup

	if m.activeFlows() == 0 {
		return
	}
	m.log.Info("draining calls", "count", m.activeFlows())

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.log.Warn("drain deadline reached", "still_active", m.activeFlows())
			return
		case <-ticker.C:
			// Re-aborted every tick, so a flow registered after the first sweep still
			// gets a terminal status. Abort is idempotent.
			m.abortAll()
			if m.activeFlows() == 0 {
				m.log.Info("drain complete")
				return
			}
		}
	}
}

func (m *Manager) abortAll() {
	m.mu.RLock()
	flows := make([]*Flow, 0, len(m.flows))
	for _, f := range m.flows {
		flows = append(flows, f)
	}
	m.mu.RUnlock()

	for _, f := range flows {
		f.Abort()
	}
}

func (m *Manager) forget(callID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.flows, callID)
}

// render synthesizes every prompt the call might need, in one round trip.
func (m *Manager) render(ctx context.Context, req voice.Request) (media, error) {
	// Order matters: the results come back in the order the segments went out.
	groups := [][]voice.Segment{
		req.Gate,
		req.Announcement,
		req.Prompt,
		req.Responses.Acknowledged,
		req.Responses.Escalated,
		req.Responses.Conference,
		req.Responses.InvalidKey,
	}

	var flat []voice.Segment
	for _, g := range groups {
		flat = append(flat, g...)
	}
	if len(flat) == 0 {
		return media{}, errors.New("nothing to say")
	}

	uris, err := m.tts.Render(ctx, flat, req.Voice)
	if err != nil {
		return media{}, err
	}
	if len(uris) != len(flat) {
		return media{}, fmt.Errorf("synthesizer returned %d media for %d segments", len(uris), len(flat))
	}

	var out media
	targets := []*[]string{
		&out.gate,
		&out.announcement,
		&out.prompt,
		&out.acknowledged,
		&out.escalated,
		&out.conference,
		&out.invalidKey,
	}
	offset := 0
	for i, g := range groups {
		*targets[i] = uris[offset : offset+len(g)]
		offset += len(g)
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
