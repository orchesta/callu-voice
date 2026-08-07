// Package voice holds the domain model shared by the call flow, the transports
// and the adapters. It depends on nothing else in this service.
package voice

import "time"

// Segment is a piece of speakable text. The caller has already applied its
// templates; this service only decides when to play it.
type Segment struct {
	Text string `json:"text"`
	Lang string `json:"lang"`
}

// Status mirrors the vocabulary the caller already understands, so a call placed
// through this service reports exactly what a cloud provider would.
type Status string

const (
	StatusAlerting            Status = "alerting"
	StatusConnected           Status = "connected"
	StatusAcknowledged        Status = "acknowledged"
	StatusEscalated           Status = "escalated"
	StatusConferenceRequested Status = "conference_requested"
	StatusVoicemail           Status = "voicemail"
	StatusNoAnswer            Status = "no_answer"
	StatusSilenceTimeout      Status = "silence_timeout"
	StatusTimeout             Status = "timeout"
	StatusFailed              Status = "failed"
)

// Keys are the digits the callee presses. Defaults are filled in by the API layer.
type Keys struct {
	Acknowledge string `json:"acknowledge"`
	Escalate    string `json:"escalate"`
	Repeat      string `json:"repeat"`
	Conference  string `json:"conference"`
}

// Limits bound a call. Every one of them exists because a call that never ends
// holds a channel, a trunk slot and an on-call engineer hostage.
type Limits struct {
	MaxDuration    time.Duration
	SilenceTimeout time.Duration
	MaxReprompts   int
}

// Responses are spoken back to the callee to confirm what their keypress did.
// Silence after a keypress leaves them unsure whether the page was acknowledged.
type Responses struct {
	Acknowledged []Segment
	Escalated    []Segment
	Conference   []Segment
	InvalidKey   []Segment
}

// Request is one outbound page.
type Request struct {
	CallID string
	To     string
	Voice  string
	// Gate is an incident-free prompt played on answer; the announcement follows only after a keypress,
	// so voicemail never records incident detail. Optional — empty plays the announcement immediately.
	Gate         []Segment
	Announcement []Segment // preamble and incident detail; played after the gate
	Prompt       []Segment // the key menu; replayed on repeat, invalid key and silence
	Responses    Responses
	Keys         Keys
	Limits       Limits
	CallbackURL  string
}

// SegmentCount is every segment the request will ask the synthesizer to render, in one number.
// They go out as a single call, so the synthesizer's cap applies to the total, not to any group.
func (r Request) SegmentCount() int {
	return len(r.Gate) + len(r.Announcement) + len(r.Prompt) +
		len(r.Responses.Acknowledged) + len(r.Responses.Escalated) +
		len(r.Responses.Conference) + len(r.Responses.InvalidKey)
}

// Event is what the caller is told about a call, as it happens.
type Event struct {
	CallID    string            `json:"call_id"`
	Status    Status            `json:"status"`
	Duration  float64           `json:"duration_s"`
	Data      map[string]string `json:"data,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}
