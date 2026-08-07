// Package tts talks to callu-tts. It renders text into media ids that Asterisk
// resolves against the shared sounds volume.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

// ErrRejected means the synthesizer refused the text, voice or language. Retrying sends the same
// thing again, so the caller has to be told to change it rather than to try later.
var ErrRejected = errors.New("synthesizer rejected the request")

// ErrBusy means the synthesizer is rendering a call and refused a render nobody is waiting on a
// phone for. Retrying later is the right move; the text is fine.
var ErrBusy = errors.New("synthesizer is rendering a call")

// Bounds the synthesizer enforces. Kept here so an over-long incident description is clipped
// before it becomes a refusal, rather than after.
const (
	MaxSegmentChars = 2_000
	MaxSegments     = 32
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

type request struct {
	Segments      []voice.Segment `json:"segments"`
	Voice         string          `json:"voice"`
	Discretionary bool            `json:"discretionary,omitempty"`
}

type response struct {
	Media   []string `json:"media"`
	Results []result `json:"results"`
}

type result struct {
	Sound      string  `json:"sound"`
	Normalized string  `json:"normalized"`
	DurationS  float64 `json:"duration_s"`
}

// PreviewSegment is what a segment turned into, without anyone having to listen: Normalized is the
// text the synthesizer actually spoke, which is where a wrong language or an unread number shows up.
type PreviewSegment struct {
	Key        string  `json:"key"`
	Normalized string  `json:"normalized"`
	DurationS  float64 `json:"duration_s"`
}

// Healthy reports whether the synthesizer can serve. The model warms asynchronously, so a 200 from
// the process is not enough.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		ModelLoaded bool `json:"model_loaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.ModelLoaded
}

// Render returns one media URI per segment, in order.
func (c *Client) Render(ctx context.Context, segments []voice.Segment, voiceName string) ([]string, error) {
	out, err := c.synthesize(ctx, segments, voiceName, false)
	if err != nil {
		return nil, err
	}
	return out.Media, nil
}

// Preview renders the same way a call does and reports what each segment became, so the text can be
// checked without placing one. Marked discretionary: a phone ringing comes first.
func (c *Client) Preview(ctx context.Context, segments []voice.Segment, voiceName string) ([]PreviewSegment, error) {
	out, err := c.synthesize(ctx, segments, voiceName, true)
	if err != nil {
		return nil, err
	}

	previews := make([]PreviewSegment, 0, len(out.Results))
	for _, r := range out.Results {
		previews = append(previews, PreviewSegment{
			Key:        r.Sound[strings.LastIndex(r.Sound, "/")+1:],
			Normalized: r.Normalized,
			DurationS:  r.DurationS,
		})
	}
	return previews, nil
}

// Audio streams one rendered segment as a WAV. The caller closes the body.
func (c *Client) Audio(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/audio/"+url.PathEscape(key), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("%w: audio %s", ErrRejected, key)
		}
		return nil, fmt.Errorf("tts returned %d for audio %s", resp.StatusCode, key)
	}
	return resp.Body, nil
}

func (c *Client) synthesize(
	ctx context.Context, segments []voice.Segment, voiceName string, discretionary bool,
) (*response, error) {
	body, err := json.Marshal(request{Segments: segments, Voice: voiceName, Discretionary: discretionary})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/synthesize", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var detail struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&detail)

		// A 4xx here is the caller's text, voice or language — not a fault of ours. Kept apart so
		// the call API can answer 400 instead of a 502 the caller would retry forever.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: %s", ErrRejected, detail.Detail)
		}

		// Only a discretionary render is ever turned away for the moment rather than the text, and
		// its caller can be told to come back instead of being shown a fault.
		if discretionary && resp.StatusCode == http.StatusServiceUnavailable {
			return nil, fmt.Errorf("%w: %s", ErrBusy, detail.Detail)
		}
		return nil, fmt.Errorf("tts returned %d: %s", resp.StatusCode, detail.Detail)
	}

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
