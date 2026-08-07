// Package telephony adapts Asterisk's ARI to the small interface the call flow
// needs. Nothing above this package knows Asterisk exists.
package telephony

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CyCoreSystems/ari/v6"
	"github.com/CyCoreSystems/ari/v6/client/native"
	"github.com/google/uuid"

	"github.com/orchesta/callu-voice/internal/voice"
)

const (
	callIDVar = "CALLU_CALL_ID"
	amdVar    = "CALLU_AMD"

	// Per-operation ceiling. Without it a single stuck ARI call blocks the flow
	// goroutine outside its select, so the call's MaxDuration is never observed
	// and the channel is held indefinitely.
	opTimeout = 10 * time.Second

	// How often the event pump checks the connection is still alive.
	heartbeat = 10 * time.Second

	// Heartbeats between full re-reads of the trunk endpoint. The state arrives by event; this
	// only covers an event missed while the socket was re-establishing.
	trunkRefreshTicks = 6
)

type Options struct {
	Application  string
	URL          string
	WebsocketURL string
	Username     string
	Password     string

	// EndpointFormat turns a number into a channel, e.g. "PJSIP/%s@trunk".
	EndpointFormat string

	// PJSIP endpoint to watch. Empty means there is no carrier to report on.
	TrunkEndpoint string

	// Outbound calls run this dialplan before entering Stasis. It is where
	// answering-machine detection happens.
	Context   string
	Extension string

	DetectMachine bool
	CallerID      string
	DialTimeout   time.Duration
}

type ARI struct {
	client ari.Client
	opts   Options
	log    *slog.Logger

	// Whether the event pump is running and the connection is believed alive.
	// The health endpoint reads this so it cannot report ready while paging is
	// silently broken.
	connected atomic.Bool

	// Last thing Asterisk said about the trunk. Nil until the first reading, and again whenever
	// the carrier changes: the endpoint the reading came from no longer exists.
	trunk atomic.Pointer[trunkStatus]

	// Whether a carrier is configured, as last stated by whatever applied one. This service holds
	// none until the caller puts one here, so false is also the truth at startup.
	carrier atomic.Bool

	mu    sync.RWMutex
	calls map[string]binding // by channel id
}

type trunkStatus struct {
	state string
	// Whether the endpoint has ever been seen online. An endpoint reads "offline" until its
	// first qualify completes, so before that the word means "not asked yet".
	seenOnline bool
}

// binding is what the adapter knows about a channel it originated.
type binding struct {
	callID string
	// Whether the channel has reached the application; before that the dialplan is still running.
	inStasis bool
}

func Connect(ctx context.Context, opts Options, log *slog.Logger) (*ARI, error) {
	client, err := native.Connect(&native.Options{
		Application:  opts.Application,
		Username:     opts.Username,
		Password:     opts.Password,
		URL:          opts.URL,
		WebsocketURL: opts.WebsocketURL,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to asterisk: %w", err)
	}
	return &ARI{
		client: client,
		opts:   opts,
		log:    log,
		calls:  make(map[string]binding),
	}, nil
}

func (a *ARI) Close() {
	a.client.Close()
}

func (a *ARI) Originate(ctx context.Context, callID, to string) (voice.ChannelID, error) {
	channelID := uuid.NewString()

	// Registered before the channel exists: an event can otherwise beat the
	// bookkeeping and be dropped as unknown.
	a.mu.Lock()
	a.calls[channelID] = binding{callID: callID}
	a.mu.Unlock()

	req := ari.OriginateRequest{
		ChannelID: channelID,
		Endpoint:  fmt.Sprintf(a.opts.EndpointFormat, to),
		Context:   a.opts.Context,
		Extension: a.opts.Extension,
		Priority:  1,
		CallerID:  a.opts.CallerID,
		Timeout:   int(a.opts.DialTimeout.Seconds()),
		Variables: map[string]string{
			callIDVar: callID,
			amdVar:    boolString(a.opts.DetectMachine),
		},
	}

	_, err := bounded(ctx, func() (struct{}, error) {
		_, e := a.client.Channel().Originate(nil, req)
		return struct{}{}, e
	})
	if err != nil {
		// Asterisk may have created the channel even though we never read the answer.
		// Async so a hard ARI failure does not slow the caller down; debug, not warn,
		// because the ordinary case is that there is no channel to hang up.
		go func() {
			if hangupErr := a.Hangup(context.WithoutCancel(ctx), voice.ChannelID(channelID)); hangupErr != nil {
				a.log.Debug("cleanup hangup after a failed originate did not apply",
					"channel", channelID, "call_id", callID, "error", hangupErr)
			}
		}()
		a.forget(channelID)
		return "", err
	}

	// ARI only delivers events for channels its application owns; without this subscription every
	// pre-answer event is lost, and an unanswered call reports a timeout instead of no-answer.
	appKey := ari.NewKey(ari.ApplicationKey, a.opts.Application)
	if _, err := bounded(ctx, func() (struct{}, error) {
		return struct{}{}, a.client.Application().Subscribe(appKey, "channel:"+channelID)
	}); err != nil {
		a.log.Warn("subscribe to channel failed", "channel", channelID, "error", err)
	}

	return voice.ChannelID(channelID), nil
}

func (a *ARI) Play(ctx context.Context, ch voice.ChannelID, media ...string) (voice.PlaybackID, error) {
	playbackID := uuid.NewString()
	key := ari.NewKey(ari.ChannelKey, string(ch))

	if _, err := bounded(ctx, func() (struct{}, error) {
		_, e := a.client.Channel().Play(key, playbackID, media...)
		return struct{}{}, e
	}); err != nil {
		return "", err
	}
	return voice.PlaybackID(playbackID), nil
}

func (a *ARI) Stop(ctx context.Context, pb voice.PlaybackID) error {
	_, err := bounded(ctx, func() (struct{}, error) {
		return struct{}{}, a.client.Playback().Stop(ari.NewKey(ari.PlaybackKey, string(pb)))
	})
	return err
}

func (a *ARI) Hangup(ctx context.Context, ch voice.ChannelID) error {
	_, err := bounded(ctx, func() (struct{}, error) {
		return struct{}{}, a.client.Channel().Hangup(ari.NewKey(ari.ChannelKey, string(ch)), "normal")
	})
	return err
}

func (a *ARI) Variable(ctx context.Context, ch voice.ChannelID, name string) (string, error) {
	return bounded(ctx, func() (string, error) {
		return a.client.Channel().GetVariable(ari.NewKey(ari.ChannelKey, string(ch)), name)
	})
}

// TrunkState reports what Asterisk last said about the endpoint outbound calls leave through.
// degraded turns true only once the endpoint has been seen online and then stopped being so; a
// carrier that was removed on purpose reads unconfigured, which is a state, not a fault.
func (a *ARI) TrunkState() (state string, degraded bool) {
	if a.opts.TrunkEndpoint == "" {
		return "unwatched", false
	}
	s := a.trunk.Load()
	if s == nil {
		// Nothing has been read since the carrier last changed. Whether that is a fault depends
		// on whether there is meant to be a carrier at all.
		if a.carrier.Load() {
			return "unknown", false
		}
		return "unconfigured", false
	}
	return s.state, s.seenOnline && !online(s.state)
}

// NoteCarrier records that a carrier has just been applied or removed. Either way the endpoint
// object is a new one, so the previous one's readings are dropped rather than held against it.
func (a *ARI) NoteCarrier(configured bool) {
	a.carrier.Store(configured)
	a.trunk.Store(nil)
	a.log.Info("carrier changed", "configured", configured, "endpoint", a.opts.TrunkEndpoint)
}

func online(state string) bool { return strings.EqualFold(state, "online") }

// noteTrunkState records a reading for the watched endpoint and ignores every other one.
func (a *ARI) noteTrunkState(resource, state string) {
	if a.opts.TrunkEndpoint == "" || !strings.EqualFold(resource, a.opts.TrunkEndpoint) {
		return
	}

	previous := a.trunk.Load()
	seen := online(state) || (previous != nil && previous.seenOnline)
	a.trunk.Store(&trunkStatus{state: state, seenOnline: seen})

	if previous == nil || previous.state != state {
		a.log.Info("trunk state", "endpoint", resource, "state", state)
	}
}

// noteTrunkUnreadable drops an "online" reading the endpoint can no longer confirm. Replacing
// the carrier deletes that object, and no event ever arrives for one that no longer exists.
func (a *ARI) noteTrunkUnreadable() {
	previous := a.trunk.Load()
	if previous == nil || !online(previous.state) {
		return
	}
	a.trunk.Store(&trunkStatus{state: "unknown", seenOnline: previous.seenOnline})
	a.log.Warn("trunk endpoint could not be read; treating the last online reading as stale",
		"endpoint", a.opts.TrunkEndpoint)
}

// watchTrunk subscribes the application to the trunk endpoint and takes a first reading, so
// /health has an answer before any event arrives.
func (a *ARI) watchTrunk(ctx context.Context) {
	if a.opts.TrunkEndpoint == "" {
		a.log.Info("no trunk endpoint to watch", "endpoint_format", a.opts.EndpointFormat)
		return
	}

	appKey := ari.NewKey(ari.ApplicationKey, a.opts.Application)
	if _, err := bounded(ctx, func() (struct{}, error) {
		return struct{}{}, a.client.Application().Subscribe(appKey, "endpoint:PJSIP/"+a.opts.TrunkEndpoint)
	}); err != nil {
		// Not fatal: the periodic re-read below still reports the state, just less promptly.
		a.log.Warn("subscribe to trunk endpoint failed", "endpoint", a.opts.TrunkEndpoint, "error", err)
	}
	a.refreshTrunk(ctx)
}

// refreshTrunk re-reads the endpoint, so a state change whose event was missed while the socket
// was down does not sit stale until the next one.
func (a *ARI) refreshTrunk(ctx context.Context) {
	if a.opts.TrunkEndpoint == "" {
		return
	}
	data, err := bounded(ctx, func() (*ari.EndpointData, error) {
		return a.client.Endpoint().Data(ari.NewEndpointKey("PJSIP", a.opts.TrunkEndpoint))
	})
	if err != nil {
		a.log.Debug("could not read trunk endpoint state", "error", err)
		a.noteTrunkUnreadable()
		return
	}
	a.noteTrunkState(data.Resource, data.State)
}

// Ready reports whether the service can place a call right now. Used by the
// health endpoint. false here must make the endpoint return 503.
func (a *ARI) Ready(ctx context.Context) (bool, string) {
	if !a.connected.Load() {
		return false, "ari event pump down"
	}
	if _, err := bounded(ctx, func() (struct{}, error) {
		_, e := a.client.Asterisk().Info(nil)
		return struct{}{}, e
	}); err != nil {
		return false, "asterisk unreachable"
	}
	return true, ""
}

// Run pumps Asterisk events into the domain. It returns when the context is cancelled or the
// connection is lost; a lost connection is fatal and the caller must exit.
func (a *ARI) Run(ctx context.Context, dispatch func(voice.TelephonyEvent)) {
	a.connected.Store(true)
	defer a.connected.Store(false)

	sub := a.client.Bus().Subscribe(nil,
		ari.Events.StasisStart,
		ari.Events.ChannelStateChange,
		ari.Events.ChannelDtmfReceived,
		ari.Events.PlaybackFinished,
		ari.Events.ChannelDestroyed,
		ari.Events.EndpointStateChange,
		ari.Events.ContactStatusChange,
	)
	defer sub.Cancel()

	a.watchTrunk(ctx)

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	ticks := 0

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			if !a.alive(ctx) {
				a.log.Error("ari connection lost; exiting event pump")
				return
			}
			ticks++
			if ticks%trunkRefreshTicks == 0 {
				a.refreshTrunk(ctx)
			}

		case e, ok := <-sub.Events():
			if !ok {
				a.log.Error("ari event stream closed; exiting event pump")
				return
			}
			if ev, ok := a.translate(e); ok {
				dispatch(ev)
			}
		}
	}
}

// alive checks the websocket state (where the client exposes it) and REST reachability, bounded so
// a socket that never answers cannot block the heartbeat.
func (a *ARI) alive(ctx context.Context) bool {
	if c, ok := a.client.(interface{ Connected() bool }); ok && !c.Connected() {
		return false
	}
	_, err := bounded(ctx, func() (struct{}, error) {
		_, e := a.client.Asterisk().Info(nil)
		return struct{}{}, e
	})
	return err == nil
}

func (a *ARI) translate(e ari.Event) (voice.TelephonyEvent, bool) {
	switch v := e.(type) {
	case *ari.StasisStart:
		a.enterStasis(v.Channel.ID)
		return a.event(voice.EventAnswered, v.Channel.ID), true

	case *ari.ChannelStateChange:
		if v.Channel.State != "Ringing" && v.Channel.State != "Ring" {
			return voice.TelephonyEvent{}, false
		}
		return a.event(voice.EventRinging, v.Channel.ID), true

	case *ari.ChannelDtmfReceived:
		// A keypress from before the channel reached us is not a menu selection: nothing has been
		// read out yet. Answering-machine detection makes that window seconds long, and a far-end
		// auto-attendant can send digits in early media without anyone ever picking up.
		if !a.inStasis(v.Channel.ID) {
			a.log.Info("ignoring a keypress from before the call was answered",
				"channel", v.Channel.ID, "digit", v.Digit)
			return voice.TelephonyEvent{}, false
		}
		ev := a.event(voice.EventDTMF, v.Channel.ID)
		ev.Digit = v.Digit
		return ev, true

	case *ari.PlaybackFinished:
		ev := a.event(voice.EventPlaybackFinished, targetChannel(v.Playback.TargetURI))
		ev.Playback = voice.PlaybackID(v.Playback.ID)
		return ev, true

	case *ari.ChannelDestroyed:
		ev := a.event(voice.EventHungUp, v.Channel.ID)
		ev.Cause = v.CauseTxt
		a.forget(v.Channel.ID)
		return ev, true

	// Neither is a call event; both are how the carrier's reachability reaches /health.
	case *ari.EndpointStateChange:
		a.noteTrunkState(v.Endpoint.Resource, v.Endpoint.State)

	case *ari.ContactStatusChange:
		a.noteTrunkState(v.Endpoint.Resource, v.Endpoint.State)
	}
	return voice.TelephonyEvent{}, false
}

func (a *ARI) event(kind voice.TelephonyEventKind, channelID string) voice.TelephonyEvent {
	a.mu.RLock()
	callID := a.calls[channelID].callID
	a.mu.RUnlock()

	return voice.TelephonyEvent{
		Kind:      kind,
		CallID:    callID,
		ChannelID: voice.ChannelID(channelID),
	}
}

// enterStasis records that the channel has reached the application.
func (a *ARI) enterStasis(channelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.calls[channelID]; ok {
		b.inStasis = true
		a.calls[channelID] = b
	}
}

// inStasis reports whether the channel has reached the application. An unknown channel is not in
// it — a channel this adapter never originated has no menu of ours to answer either.
func (a *ARI) inStasis(channelID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.calls[channelID].inStasis
}

func (a *ARI) forget(channelID string) {
	a.mu.Lock()
	delete(a.calls, channelID)
	a.mu.Unlock()
}

// bounded runs a synchronous ARI call under a per-operation timeout. The native client ignores
// context, so the call is abandoned in a goroutine rather than cancelled.
func bounded[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	type result struct {
		value T
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()

	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case r := <-ch:
		return r.value, r.err
	}
}

// targetChannel extracts the channel id from a playback target such as
// "channel:1748c0d1-...".
func targetChannel(uri string) string {
	return strings.TrimPrefix(uri, "channel:")
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
