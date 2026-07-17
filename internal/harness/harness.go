// Package harness abstracts a coding-agent CLI (Claude Code, Codex, ...) behind
// the small contract the loop needs. Each method maps to a row of the capability
// table in the design memo (docs/proposals/looping.md). Capabilities that not
// every harness supports — usage introspection — return a sentinel so the loop
// degrades gracefully rather than assuming.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Adapter is one harness. Implementations register themselves via Register in an
// init() so the loop can select by name.
type Adapter interface {
	// Name is the harness identifier ("claude", "codex").
	Name() string

	// Invoke runs one fresh, non-interactive session and returns its outcome.
	// This is the process the loop respawns each iteration; it must honour ctx
	// cancellation (Ctrl-C / SIGTERM / a supervised-wait deadline).
	Invoke(ctx context.Context, in Invocation) (Result, error)

	// DetectLimit decides whether a finished Result died on the subscription usage
	// cap (5-hour / weekly), and if so when it is expected to reset (best-effort;
	// the reset is an upper bound, not a wake signal — the loop polls for an early
	// reset). This is the long-pause case, distinct from a transient blip.
	DetectLimit(Result) Limit

	// IsTransient reports whether a non-zero exit is a retryable server/network
	// blip (API 5xx/408/429, overloaded, connection reset, ...) rather than a real
	// failure — so the loop backs off and retries the same iteration instead of
	// dying. Detection is anchored (e.g. on Claude's "API Error:" prefix) so a task
	// log that merely mentions an HTTP 500 is not mistaken for a dead session.
	IsTransient(Result) bool

	// Probe runs the cheapest possible request to answer "am I still limited?"
	// without doing real work — used while paused to catch an early reset.
	Probe(ctx context.Context, in Invocation) (Limit, error)

	// ReadUsage returns current window usage if the harness exposes it headless.
	// NO harness does today (see the memo), so implementations return
	// ErrUsageUnsupported and the loop falls back to the self-accounted Budget.
	// Kept in the contract because it is the right seam the day one adds it.
	ReadUsage(ctx context.Context, in Invocation) (Usage, error)
}

// Invocation is everything a harness needs to run one session. The loop builds it
// from config; adapters translate it into their own CLI dialect.
type Invocation struct {
	Prompt        string
	Model         string
	WorkDir       string
	MCPConfigPath string
	// ConfigDir sets the harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME) so a
	// headless session loads the same skills, plugins, and auth as the interactive
	// one. Empty inherits the ambient environment.
	ConfigDir string
	// SettingsPath is an extra settings file (Claude Code --settings) carrying the
	// headless permission policy. Merges with the config-dir's settings; deny wins.
	// Empty = no extra file. Claude-specific; other adapters ignore it.
	SettingsPath string
	// Console is where the adapter streams live, human-readable progress (the
	// terminal and/or a per-iteration logfile). Nil → os.Stderr.
	Console io.Writer
	Env     []string // extra env, appended to the process environment
	// Probe marks this as a cheap liveness check, not real work — adapters run
	// the smallest possible request instead of the drain prompt.
	Probe bool
}

// Result is the outcome of one session, both raw and parsed.
type Result struct {
	ExitCode     int
	Stdout       string
	Stderr       string
	FinalMessage string         // the agent's final message, when parseable
	Tokens       int            // tokens this session consumed (for the Budget)
	CostUSD      float64        // $ this session consumed
	Raw          map[string]any // adapter-specific parsed fields
}

// Limit describes a usage/rate-limit state.
type Limit struct {
	Limited bool
	ResetAt time.Time // zero = unknown
	Reason  string
}

// Usage is current window consumption, when a harness can report it headless.
type Usage struct {
	FiveHourUsedPct float64
	WeeklyUsedPct   float64
	ResetAt         time.Time
}

// ErrUsageUnsupported is returned by ReadUsage on harnesses without headless
// quota introspection — which, today, is all of them.
var ErrUsageUnsupported = errors.New("usage introspection not supported by this harness")

var registry = map[string]Adapter{}

// Register adds an adapter to the registry (called from adapter init()s).
func Register(a Adapter) { registry[a.Name()] = a }

// Get resolves an adapter by name.
func Get(name string) (Adapter, error) {
	if a, ok := registry[name]; ok {
		return a, nil
	}
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("unknown harness %q (have: %s)", name, strings.Join(names, ", "))
}
