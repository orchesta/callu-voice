// Package notify delivers call status back to the caller.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/orchesta/callu-voice/internal/voice"
)

// Notifier posts events to the callback URL supplied with each call. Delivery is retried a
// bounded number of times; a dropped "acknowledged" leaves an incident paging someone who answered.
type Notifier struct {
	http     *http.Client
	attempts int
	backoff  time.Duration
	log      *slog.Logger
}

func New(timeout time.Duration, attempts int, backoff time.Duration, allowPrivate bool, log *slog.Logger) *Notifier {
	transport := &http.Transport{
		// The address here is already resolved to an IP, so checking it at connect
		// time defeats DNS rebinding: a hostname that resolves to a public IP for
		// the allowlist check but to 127.0.0.1 for the actual dial is caught.
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: guardDial(allowPrivate),
		}).DialContext,
	}
	return &Notifier{
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			// A callback that redirects could bounce off the allowlist into a
			// blocked target; refuse to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("callback redirects are not allowed")
			},
		},
		attempts: attempts,
		backoff:  backoff,
		log:      log,
	}
}

// guardDial blocks the usual SSRF targets: loopback, link-local (cloud metadata at 169.254.169.254),
// unspecified and multicast. Private ranges are allowed unless allowPrivate is false.
func guardDial(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("callback host did not resolve to an IP")
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
			return fmt.Errorf("callback to non-routable address %s is blocked", ip)
		}
		if !allowPrivate && ip.IsPrivate() {
			return fmt.Errorf("callback to private address %s is blocked", ip)
		}
		return nil
	}
}

// Budget is the longest a single Notify can take: every attempt timing out, plus the backoff between them.
func (n *Notifier) Budget() time.Duration {
	total := time.Duration(n.attempts) * n.http.Timeout
	for attempt := 1; attempt < n.attempts; attempt++ {
		total += n.backoff * time.Duration(attempt)
	}
	return total
}

func (n *Notifier) Notify(ctx context.Context, callbackURL string, event voice.Event) error {
	if callbackURL == "" {
		return nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= n.attempts; attempt++ {
		if err := n.post(ctx, callbackURL, body); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if attempt == n.attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.backoff * time.Duration(attempt)):
		}
	}
	return fmt.Errorf("after %d attempts: %w", n.attempts, lastErr)
}

func (n *Notifier) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 4xx will not improve on retry, but the loop is short enough that
	// distinguishing them buys little; treat any non-2xx as a failure.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback returned %d", resp.StatusCode)
	}
	return nil
}
