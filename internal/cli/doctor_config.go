package cli

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 1. config ---------------------------------------------------------------

// apiKeyOriginLabel prefixes the line naming the one origin the account-scoped
// key may be sent to. A const so the README-pins coupling test (readme_pins_test.go,
// CLA-383) can assert the README quotes exactly this label: rewording it here fails
// that test until the README is updated.
const apiKeyOriginLabel = "api key origin: "

func checkConfig(cfg *config.Config) check {
	c := check{name: "config", status: pass}
	if src := cfg.Source(); src != "" {
		c.detail = "loaded " + src
	} else {
		// Flags-only is a legitimate way to run, not a missing-file problem — say
		// so plainly rather than implying something is absent.
		c.detail = "no config file — defaults plus flags"
	}

	workdir := cfg.WorkDir
	if workdir == "" {
		workdir = "(current directory)"
	}
	c.info = append(c.info, "harness: "+cfg.Harness, "workdir: "+workdir)
	// The one setting that changes what the terminal SHOWS rather than what the
	// daemon does (CLA-701): with it on, a session's whole stream is mirrored to
	// the terminal again, which is worth knowing before an operator wonders why
	// their overnight pane is unreadable.
	if cfg.EchoSessions {
		c.info = append(c.info, "echo_sessions: true - session streams are echoed to the terminal as well as their iteration logs")
	}
	// Which phase runs where, but only when that is not the same answer twice: a
	// mixed sequence is the one config where "harness: <x>" above is true and
	// incomplete, and the pairing is what an operator wants to eyeball before an
	// overnight run - not that two harnesses are involved, but which way round.
	if len(cfg.PhaseHarnesses()) > 1 {
		pairs := make([]string, 0, len(cfg.Phases))
		for i, ph := range cfg.EffectivePhases() {
			pairs = append(pairs, ph.Label(i)+" -> "+ph.Harness)
		}
		c.info = append(c.info, "phase harnesses: "+strings.Join(pairs, ", "))
	}
	// CLA-639: the review panel, by member and model, whenever the config sets
	// one, or the single reviewer when it does not. The panel is OPT-IN, so an
	// operator who never wrote `review_panel` has to be able to see which single
	// reviewer their phases will run. Empty when the sequence has no built-in,
	// non-final review phase, because then no review gate runs at all.
	if panel := panelLabels(cfg); len(panel) > 0 {
		// CLA-697: the members run concurrently by default; say when the cap
		// makes that not the whole panel, because a sequential (cap 1) or
		// throttled panel is a choice that changes the round's wall time.
		line := "review panel: " + strings.Join(panel, ", ")
		switch n := cfg.EffectivePanelConcurrency(len(panel)); {
		case n == 1:
			line += " (sequential)"
		case n < len(panel):
			line += fmt.Sprintf(" (at most %d at once)", n)
		}
		c.info = append(c.info, line)
	} else if r := singleReviewerLabel(cfg); r != "" {
		c.info = append(c.info, "review: single reviewer ("+r+")")
	}
	// The one destination the account-scoped key is allowed to reach (CLA-257).
	// Named here so the preflight answers "where does my credential go" without the
	// operator having to reason about which file won.
	c.info = append(c.info, apiKeyOriginLabel+orNone(cfg.CredentialOrigin()))
	// What this config hands the child process, by NAME only - never a value, and
	// never the file an @path names. A config that reaches the loop decides the
	// spawned session's environment (CLA-260), so "which variables am I injecting"
	// should be answerable from the preflight rather than by re-reading the file.
	// Since CLA-462 declarations live at four levels; each line names its level.
	if decls := cfg.DeclaredEnvs(); len(decls) > 0 {
		for _, line := range rangeEnvNames(decls) {
			c.info = append(c.info, line)
		}
	}
	// ...and what the session may INHERIT, which since CLA-714 is a config
	// decision rather than the daemon's whole environment. Same rule as above:
	// names only, never values.
	if cfg.EnvAllowlistDeclared() {
		for _, line := range rangeEnvAllowlist(cfg) {
			c.info = append(c.info, line)
		}
		c.info = append(c.info, "sessions inherit: the env_allowlist above where one applies, plus the env block")
	} else {
		c.info = append(c.info, "sessions inherit: the daemon's whole environment (no env_allowlist declared)")
	}
	if len(cfg.Projects) > 0 {
		for _, p := range cfg.Projects {
			// Spelled exactly like the check name below, so an operator can grep one
			// line for the other.
			c.info = append(c.info, "backlog["+p.Slug+"]: "+orNone(cfg.ProjectSummaryURL(p)))
		}
	} else {
		c.info = append(c.info, "backlog: "+orNone(cfg.BacklogSummaryURL()))
	}
	return c
}

// panelLabels renders the review panel as `role -> harness/model` entries, in
// the order the driver spawns them. It reads the EFFECTIVE sequence, so a
// `review_panel` entry's own model (CLA-639) is shown as it will actually run —
// an operator's preflight should not have to re-derive the panel from config.
//
// The role is POSITIONAL (`review-N`, counted over the review-shaped phases,
// exactly as the driver derives the durable record's role). A member's `name` is
// only its log label, so a named member is printed `review-N (label)` when the
// two differ; printing the label alone would suggest the record is keyed on it.
// A TRIAL seat is marked `(optional)`, the same marker the daemon's INCOMPLETE
// line uses (CLA-784), so the operator's preflight shows which seat is being
// evaluated and which ones the panel relies on.
//
// Empty when no panel is configured or none expands: a config whose review
// carries its own prompt, an unphased run, or the absent key (the single
// reviewer) has no panel to report rather than an empty one to apologise for.
func panelLabels(cfg *config.Config) []string {
	var out []string
	role := 0
	for i, ph := range cfg.EffectivePhases() {
		if !config.ReviewShaped(ph.Prompt) {
			continue
		}
		role++
		if !ph.IsPanelMember() {
			continue
		}
		name := "review-" + strconv.Itoa(role)
		if label := ph.Label(i); label != name {
			name += " (" + label + ")"
		}
		if ph.IsOptionalPanelMember() {
			name += " (optional)"
		}
		out = append(out, name+" -> "+reviewerHarnessModel(cfg, ph))
	}
	return out
}

// singleReviewerLabel renders the single reviewer a config runs when it sets no
// panel (CLA-639): the built-in, non-final review phase at the harness/model it
// will spawn on. Empty when no such phase exists, so an unphased config or one
// whose review runs the final combined brief reports nothing extra — matching
// how doctor reports a phase detail only when the phase is there to report.
func singleReviewerLabel(cfg *config.Config) string {
	out := cfg.EffectivePhases()
	for i, ph := range out {
		if ph.IsPanelMember() || ph.Name != config.ReviewPhaseName || i == len(out)-1 {
			continue
		}
		if ph.Prompt != config.ReviewOnlyBrief() {
			// A custom prompt owns its own contract; it is not the shipped
			// review gate doctor talks about.
			continue
		}
		return reviewerHarnessModel(cfg, ph)
	}
	return ""
}

// reviewerHarnessModel is the `<harness>/<model>` a review phase spawns on:
// HarnessFor, not ph.Harness, because a member that INHERITS its harness carries
// an empty one until something resolves it, and a report that prints
// "review-2 -> /gpt-6" names the wrong thing (or nothing) about where the
// session will run. An unresolvable model reads as the harness's own default.
func reviewerHarnessModel(cfg *config.Config, ph config.Phase) string {
	model, _ := cfg.ModelForPhase(ph)
	if model == "" {
		model = "harness default"
	}
	return cfg.HarnessFor(ph) + "/" + model
}

// rangeEnvNames renders the config's declared session env as one line per
// LEVEL that declares anything — "env: A, B", "projects[0].env_per_harness.claude: C"
// — with no values: one of them is routinely a credential, and doctor's output is
// the thing an operator pastes into an issue.
func rangeEnvNames(decls []config.DeclaredEnv) []string {
	byLabel := map[string][]string{}
	for _, d := range decls {
		byLabel[d.Label] = append(byLabel[d.Label], d.Name)
	}
	labels := make([]string, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		names := byLabel[l]
		sort.Strings(names)
		out = append(out, "env["+l+"]: "+strings.Join(names, ", "))
	}
	return out
}

// rangeEnvAllowlist renders the config's inherited-env allowlist as one line
// per LEVEL that declares anything, mirroring rangeEnvNames: names only, never
// values, because the list decides which inherited credentials reach a session.
func rangeEnvAllowlist(cfg *config.Config) []string {
	byLabel := map[string][]string{}
	add := func(label string, list []string) {
		if len(list) == 0 {
			return
		}
		names := append([]string(nil), list...)
		sort.Strings(names)
		byLabel[label] = names
	}
	add("env_allowlist", cfg.EnvAllowlist)
	for _, name := range slices.Sorted(maps.Keys(cfg.Harnesses)) {
		add("harnesses."+name+".env_allowlist", cfg.Harnesses[name].EnvAllowlist)
	}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		add(fmt.Sprintf("projects[%d].env_allowlist", i), p.EnvAllowlist)
		for _, h := range slices.Sorted(maps.Keys(p.EnvAllowlistPerHarness)) {
			add(fmt.Sprintf("projects[%d].env_allowlist_per_harness.%s", i, h), p.EnvAllowlistPerHarness[h])
		}
	}
	labels := make([]string, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l+": "+strings.Join(byLabel[l], ", "))
	}
	return out
}
