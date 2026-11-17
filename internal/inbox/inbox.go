// Package inbox is the driver's console -> agent message transport (CLA-606).
//
// The plane never holds a socket into a session. A message is stored for the run and the
// CLI polls `GET /api/runs/<runId>/inbox?after=<seq>` every 2 s with the run's own `clk_`
// credential; each unacknowledged message is written to the session's stdin, then the
// highest written seq is acknowledged so it is not offered again. The poll is a separate
// goroutine by construction: a slow or failed poll can only delay a message, never the
// session.
package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultPollInterval is the v1 cadence the operator decided on: 2 s is "live enough" for
// steering and cheap enough for one request per run per 2 s.
const DefaultPollInterval = 2 * time.Second

// requestTimeout bounds EACH poll/ack request. A hung plane call must not wedge the poller
// (and so the next message) behind it.
const requestTimeout = 5 * time.Second

// maxResponseBytes bounds one poll's body: at most 50 messages of 4 KB plus JSON overhead.
const maxResponseBytes = 512 * 1024

// Message is one console -> agent message as the CLI delivers it.
type Message struct {
	Seq  int64
	ID   string
	At   time.Time
	Body string
}

// Page is one poll's answer.
type Page struct {
	Messages []Message
	// Next is the server's suggested cursor when no message was returned.
	Next int64
	// Ended reports that the run is no longer running; the poller stops.
	Ended bool
}

// Config wires one run's poll. Origin is the trusted plane origin (no path); RunID is the
// compound `<uuid>.<secret>` credential claim_task returned.
type Config struct {
	Origin   string
	APIKey   string
	RunID    string
	Interval time.Duration
	// Client defaults to http.DefaultClient; each request carries its own timeout.
	Client *http.Client
	// Logf defaults to a no-op. The harness passes the session console's logger.
	Logf func(format string, args ...any)
}

// Client talks to one plane origin. One client may be reused across runs.
type Client struct {
	origin string
	apiKey string
	http   *http.Client
}

// NewClient builds a client for origin. A nil *http.Client gets http.DefaultClient.
func NewClient(origin, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{origin: strings.TrimRight(origin, "/"), apiKey: apiKey, http: hc}
}

func (c *Client) inboxURL(runID string, after int64) string {
	return fmt.Sprintf("%s/api/runs/%s/inbox?after=%d", c.origin, url.PathEscape(runID), after)
}

func (c *Client) ackURL(runID string) string {
	return fmt.Sprintf("%s/api/runs/%s/inbox/ack", c.origin, url.PathEscape(runID))
}

// apiError is a non-200 plane answer, kept small and legible: the status and the plane's
// error code are what the poller logs.
type apiError struct {
	status int
	code   string
}

func (e apiError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("inbox: plane answered %d (%s)", e.status, e.code)
	}
	return fmt.Sprintf("inbox: plane answered %d", e.status)
}

func decodeError(res *http.Response) error {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	_ = json.Unmarshal(raw, &body)
	return apiError{status: res.StatusCode, code: body.Error.Code}
}

// Poll reads the run's unacknowledged messages after `after`, oldest first.
func (c *Client) Poll(ctx context.Context, runID string, after int64) (Page, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.inboxURL(runID, after), nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Page{}, decodeError(res)
	}

	var body struct {
		Messages []struct {
			ID   string `json:"id"`
			Seq  int64  `json:"seq"`
			At   string `json:"at"`
			Body string `json:"body"`
		} `json:"messages"`
		Next  int64 `json:"next"`
		Ended bool  `json:"ended"`
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, maxResponseBytes))
	if err := dec.Decode(&body); err != nil {
		return Page{}, fmt.Errorf("inbox: decode poll response: %w", err)
	}

	page := Page{Next: body.Next, Ended: body.Ended, Messages: make([]Message, 0, len(body.Messages))}
	for _, m := range body.Messages {
		at, err := time.Parse(time.RFC3339, m.At)
		if err != nil {
			at = time.Time{}
		}
		page.Messages = append(page.Messages, Message{Seq: m.Seq, ID: m.ID, At: at, Body: m.Body})
	}
	return page, nil
}

// Ack moves the delivered watermark through `through`. It is a no-op on the plane when the
// watermark already covers it, so a retry after a lost response is safe.
func (c *Client) Ack(ctx context.Context, runID string, through int64) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]int64{"seq": through})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ackURL(runID), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return decodeError(res)
	}
	return nil
}

// Poller polls ONE run on a cadence. Build it with NewPoller.
type Poller struct {
	cfg    Config
	client *Client
	logf   func(format string, args ...any)
}

// NewPoller builds a poller for cfg.RunID.
func NewPoller(cfg Config) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPollInterval
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Poller{
		cfg:    cfg,
		client: NewClient(cfg.Origin, cfg.APIKey, cfg.Client),
		logf:   logf,
	}
}

// Run polls until ctx is cancelled, the run ends, or deliver refuses a message. `deliver`
// must write the message to the session's stdin and may block; a nil error means it was
// written and may be acknowledged.
//
// Delivery is at-least-once at the transport (a message written but not acked is offered
// again after a restart) and exactly-once within this run loop: the cursor advances only
// after a successful write, and a repeated seq is skipped.
func (p *Poller) Run(ctx context.Context, deliver func(Message) error) {
	var delivered, acked int64
	lastErr := ""

	// A short first delay rather than an immediate poll: the session may still be starting
	// its MCP server, and nothing is queued in the ordinary case.
	timer := time.NewTimer(p.cfg.Interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// Retry a watermark the previous cycle could not acknowledge BEFORE reading again, so
		// a message the plane keeps offering is eventually retired without being re-delivered
		// to the session (our own cursor already skips it).
		if delivered > acked {
			if err := p.client.Ack(ctx, p.cfg.RunID, delivered); err != nil {
				p.log("inbox: could not acknowledge message #%d (will retry): %v", delivered, err)
			} else {
				acked = delivered
			}
		}

		page, err := p.client.Poll(ctx, p.cfg.RunID, delivered)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Say it once per distinct failure: a plane outage must not turn the session log
			// into a 30-lines-a-minute metronome.
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				p.log("inbox: poll failed for run %s (delivery continues on recovery): %v", p.cfg.RunID, err)
			}
			timer.Reset(p.cfg.Interval)
			continue
		}
		lastErr = ""

		for _, m := range page.Messages {
			if m.Seq <= delivered {
				continue // already written in this session
			}
			if err := deliver(m); err != nil {
				p.log("inbox: message #%d was not written (%v); it stays queued for the next process", m.Seq, err)
				return
			}
			delivered = m.Seq
			// Acknowledge the write NOW, not only on the next cycle: the session can end
			// immediately after a steer (a final result), and a watermark left for the retry
			// would re-offer this message to the next process. That same session end cancels
			// ctx, so the ACK of bytes already written must not inherit the cancellation; the
			// client's requestTimeout still bounds it. A failed ACK is harmless — the
			// top-of-loop retry (and the next process) carries it.
			if err := p.client.Ack(context.WithoutCancel(ctx), p.cfg.RunID, delivered); err != nil {
				p.log("inbox: could not acknowledge message #%d (will retry): %v", delivered, err)
			} else {
				acked = delivered
			}
		}

		if page.Ended {
			// The run is closed: acknowledge what we wrote and stop. The session's own end
			// closes stdin independently.
			if delivered > acked {
				_ = p.client.Ack(ctx, p.cfg.RunID, delivered) // a retry: cancellable, so shutdown waits on at most one ACK
			}
			return
		}
		timer.Reset(p.cfg.Interval)
	}
}

func (p *Poller) log(format string, args ...any) { p.logf(format, args...) }
