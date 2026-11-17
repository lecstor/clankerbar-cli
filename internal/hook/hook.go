// Package hook turns an interactive Claude Code session's hook payload into the
// same activity events the CLI driver emits from a spawned session's stream
// (CLA-603).
//
// Claude Code waits for a PostToolUse hook process, so this path is on the
// session's critical path in a way the driver's emitter is not. Every failure is
// therefore a no-op: a malformed payload, a lock that cannot be taken, a send
// that errors or times out all log one line on stderr and return. The whole
// invocation is bounded by the lock wait plus the send timeout (Config.LockWait,
// Config.SendTimeout), and nothing is ever written to stdout — Claude Code feeds
// a hook's stdout back into the conversation.
package hook

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/events"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

const (
	// DefaultSendTimeout bounds the one HTTP POST. It is shorter than the
	// driver's 5 s: an interactive tool call is waiting behind it.
	DefaultSendTimeout = 2 * time.Second
	// DefaultLockWait bounds waiting for another hook's in-flight send. Hooks
	// for parallel tool calls contend on one per-run counter, and the lock is
	// held across the send so the plane sees ascending sequence numbers.
	DefaultLockWait = 2 * time.Second
	// DefaultCloseGrace is the emitter's shutdown bound: a little over the send
	// timeout so a timed-out post still completes the Close path.
	DefaultCloseGrace = 2500 * time.Millisecond

	// maxPayloadBytes bounds the stdin read. A Claude Code payload is a small
	// JSON object; the cap stops a mis-wired pipe from being pulled into memory.
	maxPayloadBytes = 1 << 20
)

// Config is one invocation's wiring. RunID, APIKey and Origin are required;
// the caller checks the no-op conditions (unset env, machine-local opt-out)
// before calling Emit.
type Config struct {
	// RunID is CLANKERBAR_RUN_ID: the run credential claim_task returned
	// (either the bare uuid or the compound <uuid>.<secret> form).
	RunID string
	// APIKey is CLANKERBAR_API_KEY, the same bearer the session's MCP calls use.
	APIKey string
	// Origin is the trusted plane origin from the operator's config. Never a
	// URL discovered from a checkout.
	Origin string
	// Harness is the batch's harness field. Empty means "claude".
	Harness string
	// SeqPath is the per-run sequence file. It is guarded by an exclusive lock
	// beside it, so concurrent hook processes reserve increasing numbers.
	SeqPath string

	Logf func(string, ...any)
	Now  func() time.Time

	SendTimeout time.Duration
	LockWait    time.Duration
	CloseGrace  time.Duration
}

func (c *Config) defaults() {
	if c.Harness == "" {
		c.Harness = "claude"
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = DefaultSendTimeout
	}
	if c.LockWait <= 0 {
		c.LockWait = DefaultLockWait
	}
	if c.CloseGrace <= 0 {
		c.CloseGrace = DefaultCloseGrace
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Payload is the subset of a Claude Code hook payload the mapping reads.
// PostToolUse carries all four fields. TodoWrite is a tool name inside
// PostToolUse, not a separate hook event.
type Payload struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolUseID     string          `json:"tool_use_id"`
}

// Emit reads one hook payload from stdin and sends the matching activity event.
//
// It returns whether an event was handed to the emitter; every failure is
// logged and returns false rather than an error, because the hook must exit 0.
// A non-PostToolUse payload (PreToolUse wired beside it, say) is a silent no-op,
// as is a payload naming no tool.
func Emit(cfg Config, stdin io.Reader) bool {
	cfg.defaults()

	raw, err := io.ReadAll(io.LimitReader(stdin, maxPayloadBytes))
	if err != nil {
		cfg.logf("agent events: cannot read the hook payload (%v) — no event sent", err)
		return false
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		// A hook runs after every tool call; a noisy parse line would flood the
		// session. One line only matters when the wiring itself is broken.
		cfg.logf("agent events: the hook payload is not JSON (%v) — no event sent", err)
		return false
	}
	if p.HookEventName != "" && p.HookEventName != "PostToolUse" {
		return false
	}
	if strings.TrimSpace(p.ToolName) == "" {
		return false
	}
	if cfg.SeqPath == "" {
		cfg.logf("agent events: no sequence file for this session's run — no event sent")
		return false
	}

	// Serialize the whole reserve-and-send against the other hooks of this run.
	// The plane drops any seq at or below its high-water mark, so an event that
	// reserved a number but arrived after its successor would be lost.
	unlock, ok := acquireLock(cfg.SeqPath+".lock", cfg.LockWait)
	if !ok {
		cfg.logf("agent events: another hook held the sequence lock for over %s — dropped this event", cfg.LockWait)
		return false
	}
	defer unlock()

	counter := events.NewSeqCounter(seqFileStore{path: cfg.SeqPath}, cfg.Logf)
	emitter := events.New(events.Config{
		Origin:      cfg.Origin,
		APIKey:      cfg.APIKey,
		Harness:     cfg.Harness,
		Seq:         counter,
		Logf:        cfg.Logf,
		Now:         cfg.Now,
		SendTimeout: cfg.SendTimeout,
		CloseGrace:  cfg.CloseGrace,
	})
	emitter.SetRunID(cfg.RunID)
	harness.NoteClaudeToolEvent(emitter, p.ToolName, p.ToolUseID, p.ToolInput)
	emitter.Close()
	return true
}

func (c Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}
