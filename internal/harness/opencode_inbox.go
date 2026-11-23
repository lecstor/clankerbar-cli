package harness

// Console -> agent message delivery for the opencode adapter.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/inbox"
)

// opencodePromptTimeout bounds ONE prompt POST. The call only enqueues a steer on the local server,
// so a slow answer means the server is wedged; the poller must not wait on it forever, and an
// unanswered steer is re-offered anyway because it was never ACKed.
const opencodePromptTimeout = 10 * time.Second

// opencodeInboxRetryInterval is the default pause before re-polling a run whose last delivery
// failed.
const opencodeInboxRetryInterval = 2 * time.Second

// opencodeInbox is one Invocation's console -> agent delivery: the run-id channel Invoke feeds from
// ResumeClaim/OnClaim, the plane half of the poll, and the live process target runSession attaches
// while it runs. The goroutine starts on the FIRST attach, not at construction, so a session that
// never gets a private server (a probe, or one whose config declares no clankerbar MCP server) runs
// no delivery goroutine at all.
type opencodeInbox struct {
	ctx     context.Context
	cancel  context.CancelFunc
	console io.Writer
	cfg     InboxConfig
	runIDs  <-chan string
	client  *http.Client
	// retry is the pause before re-polling a run whose delivery failed; the default is
	// opencodeInboxRetryInterval, and tests shrink it.
	retry time.Duration

	// mu guards the target and the start state. Attach/detach/setSession are called from the run's own
	// goroutine; the delivery goroutine only reads.
	mu  sync.Mutex
	srv *opencodeServer
	sid string
	// changed is closed and replaced on every target change, so a waiter wakes on the change instead
	// of polling.
	changed chan struct{}
	started bool
	done    chan struct{}
}

// newOpencodeInbox builds one Invocation's delivery. A nil console discards the progress lines; the
// goroutine itself starts lazily (see attach).
func newOpencodeInbox(ctx context.Context, console io.Writer, cfg InboxConfig, runIDs <-chan string) *opencodeInbox {
	if console == nil {
		console = io.Discard
	}
	dctx, cancel := context.WithCancel(ctx)
	return &opencodeInbox{
		ctx:     dctx,
		cancel:  cancel,
		console: console,
		cfg:     cfg,
		runIDs:  runIDs,
		client:  &http.Client{},
		retry:   opencodeInboxRetryInterval,
		changed: make(chan struct{}),
	}
}

// attach points delivery at the private server of the process about to run and starts the delivery
// goroutine on first use.
func (d *opencodeInbox) attach(srv *opencodeServer) {
	if d == nil || srv == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.srv = srv
	d.sid = ""
	if !d.started {
		d.started = true
		d.done = make(chan struct{})
		go d.run()
	}
	d.broadcastLocked()
}

// setSession names the opencode session this process streams. A no-op when it has not changed:
// every event of a run repeats the same id.
func (d *opencodeInbox) setSession(sid string) {
	if d == nil || sid == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sid == sid {
		return
	}
	d.sid = sid
	d.broadcastLocked()
}

// detach clears the target this process attached, so nothing is posted to a server that is about to
// be stopped. A target belonging to a LATER process is left alone.
func (d *opencodeInbox) detach(srv *opencodeServer) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv != srv {
		return
	}
	d.srv = nil
	d.sid = ""
	d.broadcastLocked()
}

// close stops the delivery goroutine and waits for it, so a message written just before the session
// ended has its ACK in flight (bounded by inbox's own request timeout) before the adapter returns.
func (d *opencodeInbox) close() {
	if d == nil {
		return
	}
	d.cancel()
	d.mu.Lock()
	done := d.done
	d.mu.Unlock()
	if done != nil {
		<-done
	}
}

// broadcastLocked wakes every waitTarget; d.changed MUST be replaced, not merely closed, or a later
// change would panic on a second close.
func (d *opencodeInbox) broadcastLocked() {
	close(d.changed)
	d.changed = make(chan struct{})
}

// waitTarget blocks until a live server and a known session id are both available, and reports
// false only when ctx ends.
func (d *opencodeInbox) waitTarget(ctx context.Context) (*opencodeServer, string, bool) {
	for {
		d.mu.Lock()
		srv, sid, changed := d.srv, d.sid, d.changed
		d.mu.Unlock()
		if srv != nil && sid != "" {
			return srv, sid, true
		}
		select {
		case <-ctx.Done():
			return nil, "", false
		case <-changed:
		}
	}
}

// run is the delivery goroutine: one poller per observed run id, restarted with a fresh cursor when
// delivery fails, and replaced when the observed run id changes mid-session (a session may claim,
// settle, and claim again).
func (d *opencodeInbox) run() {
	defer close(d.done)
	for {
		var runID string
		select {
		case <-d.ctx.Done():
			return
		case runID = <-d.runIDs:
		}
		if runID == "" {
			continue
		}
		for {
			// Poll nothing until there is a live target. The plane keeps the message; posting it to a server
			// that has stopped is the one delivery failure with nowhere to recover from.
			if _, _, ok := d.waitTarget(d.ctx); !ok {
				return
			}
			next, failed := d.pollRun(runID)
			if d.ctx.Err() != nil {
				return
			}
			if next != "" {
				runID = next
				continue
			}
			if !failed {
				// The run ended (or its poller stopped): wait for the next run.
				break
			}
			select {
			case <-d.ctx.Done():
				return
			case next = <-d.runIDs:
				if next != "" {
					runID = next
				}
			case <-time.After(d.retry):
			}
		}
	}
}

// pollRun runs one inbox poller for runID until it stops.
func (d *opencodeInbox) pollRun(runID string) (next string, failed bool) {
	poller := inbox.NewPoller(inbox.Config{
		Origin:   d.cfg.Origin,
		APIKey:   d.cfg.APIKey,
		RunID:    runID,
		Interval: d.cfg.PollInterval,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(d.console, format+"\n", args...)
		},
	})
	runCtx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(runCtx, func(m inbox.Message) error {
			if err := d.deliver(runCtx, m); err != nil {
				// The write is ordered before close(done), which the reader below synchronises on, so this
				// needs no atomic.
				failed = true
				fmt.Fprintf(d.console, "!! console message #%d was not steered into the session (%v) - it stays queued and is retried\n", m.Seq, err)
				return err
			}
			fmt.Fprintf(d.console, "  ← console message #%d\n", m.Seq)
			return nil
		})
	}()
	select {
	case <-d.ctx.Done():
		cancel()
		<-done
		return "", false
	case <-done:
		return "", failed
	case next = <-d.runIDs:
		cancel()
		<-done
		return next, false
	}
}

// deliver posts one message to the live session.
func (d *opencodeInbox) deliver(ctx context.Context, m inbox.Message) error {
	srv, sid, ok := d.waitTarget(ctx)
	if !ok {
		return ctx.Err()
	}
	return postOpencodePrompt(ctx, d.client, srv, sid, m.Body)
}

// postOpencodePrompt steers one message into a live opencode session. Verified against the
// installed opencode 2.2.7 and its published OpenAPI (/docs/openapi.json): POST
// /api/session/{sessionID}/prompt takes {"text": ..., "delivery": "steer"|"queue"} and
// authenticates the same way the adapter's MCP preflight does (basic auth, user "opencode", the
// server's announced password).
func postOpencodePrompt(ctx context.Context, hc *http.Client, srv *opencodeServer, sid, text string) error {
	ctx, cancel := context.WithTimeout(ctx, opencodePromptTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"text": text, "delivery": "steer"})
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(srv.url, "/") + "/api/session/" + url.PathEscape(sid) + "/prompt"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("opencode", srv.password)
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// Drain so a kept-alive connection is reusable; the body is a diagnostic only.
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("POST /api/session/%s/prompt: %s%s", sid, res.Status, detailSuffix(raw))
	}
	return nil
}

// detailSuffix renders a short server body for an error message, or nothing.
func detailSuffix(raw []byte) string {
	if s := strings.TrimSpace(string(raw)); s != "" {
		return ": " + s
	}
	return ""
}
