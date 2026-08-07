package voice

import "context"

// Synthesizer turns speakable text into media the telephony layer can play.
type Synthesizer interface {
	// Render returns one media URI per segment, in order.
	Render(ctx context.Context, segments []Segment, voiceName string) ([]string, error)
}

// Notifier reports call progress back to whoever asked for the call.
type Notifier interface {
	Notify(ctx context.Context, callbackURL string, event Event) error
}

// ChannelID identifies a live call leg.
type ChannelID string

// PlaybackID identifies a running playback, so it can be stopped for barge-in.
type PlaybackID string

// Telephony is everything the call flow needs from a PBX.
type Telephony interface {
	// Originate dials `to` and tags the channel with callID so the resulting
	// events can be routed back to the right call.
	Originate(ctx context.Context, callID, to string) (ChannelID, error)

	// Play queues media on the channel. Several URIs play back to back as one
	// playback, so a single Stop cancels the lot.
	Play(ctx context.Context, ch ChannelID, media ...string) (PlaybackID, error)

	// Stop cancels a running playback. Used for barge-in.
	Stop(ctx context.Context, pb PlaybackID) error

	Hangup(ctx context.Context, ch ChannelID) error

	// Variable reads a channel variable, e.g. the result of answering-machine
	// detection.
	Variable(ctx context.Context, ch ChannelID, name string) (string, error)
}

// TelephonyEventKind enumerates what the PBX tells us about a channel.
type TelephonyEventKind int

const (
	// EventRinging means the far end is being alerted.
	EventRinging TelephonyEventKind = iota
	// EventAnswered means the channel entered our application.
	EventAnswered
	EventDTMF
	EventPlaybackFinished
	// EventHungUp means the channel is gone, whoever ended it.
	EventHungUp
)

// TelephonyEvent is one thing that happened to one channel.
type TelephonyEvent struct {
	Kind      TelephonyEventKind
	CallID    string
	ChannelID ChannelID
	Digit     string
	Cause     string

	// Playback identifies which playback finished. A playback stopped for barge-in also completes,
	// so events must be matched by id.
	Playback PlaybackID
}
