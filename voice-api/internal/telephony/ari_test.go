package telephony

import (
	"log/slog"
	"testing"

	"github.com/CyCoreSystems/ari/v6"

	"github.com/orchesta/callu-voice/internal/voice"
)

func adapter(t *testing.T) *ARI {
	t.Helper()
	return &ARI{
		log:   slog.New(slog.DiscardHandler),
		calls: make(map[string]binding),
	}
}

func dtmf(channelID, digit string) *ari.ChannelDtmfReceived {
	return &ari.ChannelDtmfReceived{
		Channel: ari.ChannelData{ID: channelID},
		Digit:   digit,
	}
}

func stasisStart(channelID string) *ari.StasisStart {
	return &ari.StasisStart{Channel: ari.ChannelData{ID: channelID}}
}

func TestDtmfBeforeStasisIsNotTranslated(t *testing.T) {
	a := adapter(t)
	a.calls["ch-1"] = binding{callID: "call-1"}

	if _, ok := a.translate(dtmf("ch-1", "1")); ok {
		t.Fatal("a keypress before the call reached the application was passed to the flow; " +
			"it would be read as an acknowledgement for a call nobody answered")
	}
}

func TestDtmfAfterStasisIsTranslated(t *testing.T) {
	a := adapter(t)
	a.calls["ch-1"] = binding{callID: "call-1"}

	if _, ok := a.translate(stasisStart("ch-1")); !ok {
		t.Fatal("StasisStart was not translated")
	}

	ev, ok := a.translate(dtmf("ch-1", "1"))
	if !ok {
		t.Fatal("a keypress after the answer was dropped; the responder could not acknowledge")
	}
	if ev.Kind != voice.EventDTMF || ev.Digit != "1" || ev.CallID != "call-1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestStasisStartMarksOnlyItsOwnChannel(t *testing.T) {
	a := adapter(t)
	a.calls["ch-1"] = binding{callID: "call-1"}
	a.calls["ch-2"] = binding{callID: "call-2"}

	a.translate(stasisStart("ch-1"))

	if _, ok := a.translate(dtmf("ch-2", "1")); ok {
		t.Fatal("one call answering opened the gate for another call that had not")
	}
}

// A channel this adapter never originated has no menu of ours to answer.
func TestDtmfOnAnUnknownChannelIsNotTranslated(t *testing.T) {
	a := adapter(t)

	if _, ok := a.translate(dtmf("ch-unknown", "1")); ok {
		t.Fatal("a keypress on an unknown channel was passed to the flow")
	}
}

// The gate is per channel and is dropped with it, so a reused id cannot inherit an open gate.
func TestForgetClosesTheGate(t *testing.T) {
	a := adapter(t)
	a.calls["ch-1"] = binding{callID: "call-1"}
	a.translate(stasisStart("ch-1"))

	a.forget("ch-1")
	a.calls["ch-1"] = binding{callID: "call-2"}

	if _, ok := a.translate(dtmf("ch-1", "1")); ok {
		t.Fatal("a new call on a reused channel id started with the gate already open")
	}
}

func TestStasisStartIsStillTheAnswerEvent(t *testing.T) {
	a := adapter(t)
	a.calls["ch-1"] = binding{callID: "call-1"}

	ev, ok := a.translate(stasisStart("ch-1"))
	if !ok || ev.Kind != voice.EventAnswered || ev.CallID != "call-1" {
		t.Fatalf("StasisStart must still report the answer: %+v ok=%v", ev, ok)
	}
}

func TestHangupStillTranslatesBeforeAnswer(t *testing.T) {
	a := adapter(t)
	a.calls["ch-1"] = binding{callID: "call-1"}

	ev, ok := a.translate(&ari.ChannelDestroyed{
		Channel:  ari.ChannelData{ID: "ch-1"},
		CauseTxt: "Normal Clearing",
	})
	if !ok || ev.Kind != voice.EventHungUp {
		t.Fatalf("a call that ends before answer must still report a hangup: %+v ok=%v", ev, ok)
	}
	if ev.Cause != "Normal Clearing" {
		t.Fatalf("the hangup cause was lost: %q", ev.Cause)
	}
}

func watching(t *testing.T, endpoint string) *ARI {
	t.Helper()
	a := adapter(t)
	a.opts.TrunkEndpoint = endpoint
	return a
}

func TestTrunkIsUnwatchedWhenNoEndpointIsConfigured(t *testing.T) {
	a := adapter(t)

	state, degraded := a.TrunkState()
	if state != "unwatched" || degraded {
		t.Fatalf("an install with no trunk reported %q degraded=%v", state, degraded)
	}
}

func TestTrunkOfflineBeforeItHasEverBeenOnlineIsNotDegraded(t *testing.T) {
	a := watching(t, "trunk")

	a.noteTrunkState("trunk", "offline")

	state, degraded := a.TrunkState()
	if state != "offline" {
		t.Fatalf("state = %q, want offline", state)
	}
	if degraded {
		t.Fatal("a cold start was reported as a dead carrier; every restart would fail readiness")
	}
}

func TestTrunkThatGoesOfflineAfterBeingOnlineIsDegraded(t *testing.T) {
	a := watching(t, "trunk")

	a.noteTrunkState("trunk", "online")
	if _, degraded := a.TrunkState(); degraded {
		t.Fatal("an online trunk was reported degraded")
	}

	a.noteTrunkState("trunk", "offline")

	state, degraded := a.TrunkState()
	if !degraded {
		t.Fatal("a carrier that stopped answering was not reported; /health would stay ok while every page fails")
	}
	if state != "offline" {
		t.Fatalf("state = %q, want offline", state)
	}
}

func TestTrunkComingBackClearsTheDegradedState(t *testing.T) {
	a := watching(t, "trunk")
	a.noteTrunkState("trunk", "online")
	a.noteTrunkState("trunk", "offline")

	a.noteTrunkState("trunk", "online")

	if _, degraded := a.TrunkState(); degraded {
		t.Fatal("a recovered carrier stayed degraded; the pod would never rejoin the service")
	}
}

// Changing the carrier deletes the endpoint the reading came from, so it can never be read
// again — and "online" for an endpoint that no longer exists is a healthy service that pages nobody.
func TestAnEndpointThatCanNoLongerBeReadStopsCountingAsOnline(t *testing.T) {
	a := watching(t, "trunk")
	a.noteTrunkState("trunk", "online")

	a.noteTrunkUnreadable()

	state, degraded := a.TrunkState()
	if state == "online" {
		t.Fatal("a trunk that cannot be read is still reported online")
	}
	if !degraded {
		t.Fatalf("state=%q was not treated as degraded, so /health stays green", state)
	}
}

// A carrier that has never been up is a cold start, not a fault: reporting degraded there would
// fail readiness on every restart of an install that has no trunk yet.
func TestAnEndpointThatWasNeverOnlineIsNotDegradedWhenItCannotBeRead(t *testing.T) {
	a := watching(t, "trunk")
	a.noteTrunkState("trunk", "offline")

	a.noteTrunkUnreadable()

	if state, degraded := a.TrunkState(); state != "offline" || degraded {
		t.Fatalf("a cold start was reported as a dead carrier: state=%q degraded=%v", state, degraded)
	}
}

// Another endpoint's state says nothing about ours, and a busy softphone must not fail readiness.
func TestAnotherEndpointsStateIsIgnored(t *testing.T) {
	a := watching(t, "trunk")
	a.noteTrunkState("trunk", "online")

	a.noteTrunkState("6001", "offline")

	if state, degraded := a.TrunkState(); degraded || state != "online" {
		t.Fatalf("an unrelated endpoint changed the trunk verdict: state=%q degraded=%v", state, degraded)
	}
}

// Removing the carrier deletes the endpoint the readings came from, so none can ever arrive
// again. Read as a carrier that stopped answering it would hold /health at 503 forever.
func TestRemovingTheCarrierIsAStateNotAFault(t *testing.T) {
	a := watching(t, "trunk")
	a.NoteCarrier(true)
	a.noteTrunkState("trunk", "online")
	a.noteTrunkState("trunk", "offline")
	if _, degraded := a.TrunkState(); !degraded {
		t.Fatal("premise: a carrier that stopped answering is degraded")
	}

	a.NoteCarrier(false)

	state, degraded := a.TrunkState()
	if degraded {
		t.Fatal("unlinking the carrier left the service unhealthy; nothing clears it short of a recreate")
	}
	if state != "unconfigured" {
		t.Fatalf("state = %q, want unconfigured", state)
	}
}

// The endpoint a new carrier builds is a new object, so the old one's history is not evidence
// against it — and until it has qualified once, offline only means "not asked yet".
func TestPuttingACarrierBackClearsTheDegradedState(t *testing.T) {
	a := watching(t, "trunk")
	a.NoteCarrier(true)
	a.noteTrunkState("trunk", "online")
	a.noteTrunkUnreadable()
	if _, degraded := a.TrunkState(); !degraded {
		t.Fatal("premise: an endpoint that can no longer be read is degraded")
	}

	a.NoteCarrier(true)

	state, degraded := a.TrunkState()
	if degraded {
		t.Fatal("putting the carrier back did not clear the previous carrier's verdict")
	}
	if state != "unknown" {
		t.Fatalf("state = %q, want unknown", state)
	}
}

func TestEndpointEventsUpdateTheTrunkAndAreNotCallEvents(t *testing.T) {
	a := watching(t, "trunk")

	if _, ok := a.translate(&ari.EndpointStateChange{
		Endpoint: ari.EndpointData{Technology: "PJSIP", Resource: "trunk", State: "online"},
	}); ok {
		t.Fatal("an endpoint event was dispatched to a call flow")
	}
	if state, _ := a.TrunkState(); state != "online" {
		t.Fatalf("EndpointStateChange did not update the trunk: %q", state)
	}

	if _, ok := a.translate(&ari.ContactStatusChange{
		Endpoint: ari.EndpointData{Technology: "PJSIP", Resource: "trunk", State: "offline"},
	}); ok {
		t.Fatal("a contact event was dispatched to a call flow")
	}
	if state, degraded := a.TrunkState(); state != "offline" || !degraded {
		t.Fatalf("ContactStatusChange did not update the trunk: state=%q degraded=%v", state, degraded)
	}
}
