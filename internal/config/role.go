package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The roles an instance may serve (studio-chat.md §2, operator decision
// 96ac5e8d). `role` is a MACHINE fact — which roles this machine serves — so it
// lives in the local file only; the plane's resolved `roles.planner` block says
// what the role does, and the machine says whether it serves it.
const (
	RoleWorker  = "worker"
	RolePlanner = "planner"
)

// DefaultPlannerWallClock is the planner turn's wall-clock cap when neither the
// role nor the run's budget states one: 900 s, the plane's own
// CONVERSATION_DEFAULT_TURN_SECONDS. The plane derives the conversation-claim
// lease from the same number (plus a grace window), so the two must agree or a
// live turn could outlive the claim that authorises its reply.
const DefaultPlannerWallClock = 900 * time.Second

// BuiltinPlannerPrompt is the shipped planner brief, used when the stored
// `roles.planner` block carries no `prompt` of its own. It stays CLI-side, the
// same way the built-in phase briefs do: the plane stores policy, never prose
// the CLI hands a model.
//
// The envelope the composer adds around it carries the per-turn facts (claim
// token, session ref, pending messages, transcript). This half must therefore
// say only what is true of EVERY planner turn.
const BuiltinPlannerPrompt = `You are this project's planner, answering an in-app conversation with the operator.
You do not work backlog tasks: never call claim_task, next_task, resume_task or submit_for_review.

Answer the operator's pending messages in ONE reply with the ` + "`reply_conversation`" + ` tool, passing
exactly the conversationId, claimToken, sessionRef and sessionHarness this brief gives you. A turn
is one reply; if the answer needs more than the body cap, file a task or write a doc and link it.

You may use the project's other tools to answer: get_task, list_tasks, search_tasks,
get_backlog_summary, the review reads, and read_doc_blocks for doc-block subjects. A decision you
record is mcp_self and the operator can overturn it. When a choice is the operator's to make, ask
with ask_question instead of guessing.

Older conversation history is reachable with ` + "`read_conversation`" + `, paging backwards with beforeSeq.
Do not repeat the transcript back to the operator; answer it.`

// ValidRole reports whether role names a role an instance may declare. Empty is
// not validated here: an absent `role` key means worker (see EffectiveRole).
func ValidRole(role string) bool {
	switch strings.TrimSpace(role) {
	case RoleWorker, RolePlanner:
		return true
	}
	return false
}

// EffectiveRole is the role this instance serves: the local `role` key, or
// worker when it names none. It is what the presence beacon reports.
func (c *Config) EffectiveRole() string {
	if r := strings.TrimSpace(c.Role); r != "" {
		return r
	}
	return RoleWorker
}

// PlannerEnabled reports whether the EFFECTIVE run config carries a planner
// role. It is a property of the resolved document, never of the local file: a
// project that ratified no planner (and inherits none) has no chat surface.
func (c *Config) PlannerEnabled() bool { return c != nil && c.Planner != nil }

// RoleAssignment is the stored `roles.planner` block as the CLI applies it: the
// harness, the model selection, the custom brief and the three per-turn bounds.
//
// It is decoded from the plane's document only (Config.Planner is `json:"-"`),
// and its harness/model are policy the operator ratified — never replaced by
// the local file's worker default. Validate deliberately does NOT check it: a
// bad planner role must not refuse a project's worker drain config, and a
// machine that serves several projects may serve a planner on only some of
// them. `doctor` and the planner loop report/refuse it at the point of use.
type RoleAssignment struct {
	Harness string
	Model   string
	Tier    string
	Prompt  string

	MaxTurns         int
	MaxWallClock     Duration
	MaxSessionTokens int
}

// PlannerSession is a resolved planner turn: exactly what the planner loop needs
// to spawn a session. Every field is either the role's own policy or the
// documented fallback, and nothing here falls back to the worker's local
// harness/model (D23's no-substitution rule).
type PlannerSession struct {
	Harness string
	// Model is the alias to run on, or "" for the harness's own default. A
	// resolved tier contributes its alias; an absent model AND tier deliberately
	// stays empty rather than inheriting the worker's model.
	Model  string
	Prompt string
	// MaxTurns is the per-turn cap. It reaches the CLI only on a harness that
	// honours it (claude); on the others it is inert, which `doctor` reports.
	MaxTurns int
	// MaxWallClock bounds one turn. Always positive: the fallback chain ends at
	// DefaultPlannerWallClock, because the plane derives the claim lease from
	// the same figure and "no cap" would let a turn outlive its claim.
	MaxWallClock time.Duration
	// MaxSessionTokens bounds the accumulated tokens of one resumed session
	// across turns; 0 means uncapped. The planner re-primes when it is spent.
	MaxSessionTokens int
}

// ErrNoPlanner means the effective run config carries no `roles.planner` (or an
// empty one), so this project has no chat surface. It is a typed value because
// both the loop and `doctor` treat it as an ordinary answer, not a failure.
var ErrNoPlanner = errors.New("no planner role configured")

// ResolvePlanner resolves the stored planner role into a session. It is strict
// where the phase chain is forgiving: a role naming an unknown harness, or a
// tier that resolves to nothing, yields an error and NO session rather than
// running on a default. A planner answering under the wrong model is a policy
// breach the operator cannot see, which is why the whole no-fallback rule
// exists.
func (c *Config) ResolvePlanner() (PlannerSession, error) {
	if !c.PlannerEnabled() {
		return PlannerSession{}, ErrNoPlanner
	}
	role := *c.Planner
	h := strings.TrimSpace(role.Harness)
	if h == "" {
		return PlannerSession{}, errors.New("roles.planner names no harness: a role must name its own, because the run-wide `harness` is machine policy and a project override would drop it")
	}
	if !harness.Known(h) {
		return PlannerSession{}, fmt.Errorf("roles.planner names harness %q, which this build does not know (want: %s)", h, strings.Join(harness.Names(), ", "))
	}
	model := strings.TrimSpace(role.Model)
	tier := strings.TrimSpace(role.Tier)
	if model != "" && tier != "" {
		// The plane refuses this pairing; a hand-edited store or a future
		// reader must not silently let one win (the phase rule's sibling).
		return PlannerSession{}, fmt.Errorf("roles.planner sets both `model` (%q) and `tier` (%q): a role names its model one way", model, tier)
	}
	if model == "" && tier != "" {
		alias, ok := c.ModelForPhase(Phase{Harness: h, Tier: tier})
		if !ok || strings.TrimSpace(alias) == "" {
			return PlannerSession{}, fmt.Errorf("roles.planner names tier %q, which resolves to no model on harness %q: no fallback model is substituted", tier, h)
		}
		model = alias
	}
	prompt := strings.TrimSpace(role.Prompt)
	if prompt == "" {
		prompt = BuiltinPlannerPrompt
	}
	maxTurns := role.MaxTurns
	if maxTurns <= 0 {
		maxTurns = c.MaxTurns
	}
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	wall := role.MaxWallClock.Duration()
	if wall <= 0 {
		wall = c.Budget.MaxWallClock.Duration()
	}
	if wall <= 0 {
		wall = DefaultPlannerWallClock
	}
	tokens := role.MaxSessionTokens
	if tokens <= 0 {
		tokens = c.Budget.MaxSessionTokens
	}
	return PlannerSession{
		Harness:          h,
		Model:            model,
		Prompt:           prompt,
		MaxTurns:         maxTurns,
		MaxWallClock:     wall,
		MaxSessionTokens: tokens,
	}, nil
}

// plannerRoleFromDoc maps the stored `roles` section onto this config. A nil
// map is an ABSENT key and turns chat off; a present map with no `planner` key
// (`roles: {}`) is the explicit opt-out and does the same. Only `planner` is
// consumed; the plane refuses unknown role names before they get here.
func plannerRoleFromDoc(doc *RunConfigDoc) *RoleAssignment {
	if doc == nil || doc.Roles == nil {
		return nil
	}
	role, ok := doc.Roles[RolePlanner]
	if !ok {
		return nil
	}
	return &RoleAssignment{
		Harness:          role.Harness,
		Model:            role.Model,
		Tier:             role.Tier,
		Prompt:           role.Prompt,
		MaxTurns:         role.MaxTurns,
		MaxWallClock:     role.MaxWallClock,
		MaxSessionTokens: role.MaxSessionTokens,
	}
}
