package harness

// Console -> agent message delivery for the claude adapter (CLA-606).
//
// A message is plain input: it is written to the session's stream-json stdin as a user turn,
// in the plane's per-run order. The poller is its own goroutine, so a slow or failed plane
// call can only delay a message — never the session. The run id can change mid-session (a
// session may claim a task, settle it, and claim another), so each observed run restarts the
// poll with a fresh cursor; the newest claim wins.

import (
	"context"
	"fmt"
	"io"

	"github.com/lecstor/clankerbar-cli/internal/inbox"
)

// runDelivery polls the current run's inbox and writes each message to input until ctx is
// cancelled or the session's stdin closes. It never returns an error to the session: a
// delivery problem is the plane's or the poller's, and the session's own stream decides when
// the work is over.
func runDelivery(ctx context.Context, console io.Writer, cfg InboxConfig, input *sessionInput, runIDs <-chan string) {
	for {
		var runID string
		select {
		case <-ctx.Done():
			return
		case runID = <-runIDs:
		}
		if runID == "" {
			continue
		}

		poller := inbox.NewPoller(inbox.Config{
			Origin:   cfg.Origin,
			APIKey:   cfg.APIKey,
			RunID:    runID,
			Interval: cfg.PollInterval,
			Logf: func(format string, args ...any) {
				fmt.Fprintf(console, format+"\n", args...)
			},
		})

		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			poller.Run(runCtx, func(m inbox.Message) error {
				if err := input.writeUser(m.Body); err != nil {
					return err
				}
				fmt.Fprintf(console, "  ← console message #%d\n", m.Seq)
				return nil
			})
		}()

		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-done:
			// The run ended or the input closed. A later claim may still arrive, so keep
			// listening until the session itself ends (ctx).
			cancel()
		}
	}
}
