package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- helpers -----------------------------------------------------------------

// okEnv is a doctorEnv where everything the world provides works, so each test
// can break exactly one thing and attribute the result to it.
func okEnv() doctorEnv {
	return doctorEnv{
		lookPath:   func(f string) (string, error) { return "/usr/local/bin/" + f, nil },
		binVersion: func(context.Context, string) (string, error) { return "1.2.3", nil },
		newPoller:  func(string, string) backlog.Poller { return fakePoller{} },
		apiKey:     "key-123",
	}
}

type fakePoller struct {
	sum backlog.Summary
	err error
}

func (p fakePoller) Poll(context.Context) (backlog.Summary, error) { return p.sum, p.err }

// pollerReturning builds a doctorEnv whose backlog read fails (or succeeds) in a
// specific way — the four not-wired cases are the point of the backlog check.
func envWithPoll(sum backlog.Summary, err error) doctorEnv {
	e := okEnv()
	e.newPoller = func(string, string) backlog.Poller { return fakePoller{sum: sum, err: err} }
	return e
}

// validCfg is a config that passes Validate, with the workdir pointed somewhere
// disposable so the workdir check writes into the test's temp dir.
func validCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Harness: "claude",
		Prompt:  "Work the backlog.",
		WorkDir: t.TempDir(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config does not validate: %v", err)
	}
	return cfg
}

// find returns the check with the given name. Checks are addressed by name
// rather than index so adding a check never silently re-points a test.
func find(t *testing.T, checks []check, name string) check {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, names(checks))
	return check{}
}

func names(checks []check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.name)
	}
	return out
}

// --- harness -----------------------------------------------------------------

func TestHarnessMissingBinaryFails(t *testing.T) {
	e := okEnv()
	e.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	c := checkHarness(context.Background(), validCfg(t), e)
	if c.status != fail {
		t.Errorf("missing harness binary: got %v, want FAIL", c.status)
	}
	if c.remedy == "" {
		t.Error("a FAIL must carry a remedy line")
	}
}

// On PATH but not runnable is a distinct failure from not being there at all — a
// broken shim starts the loop and kills its first session.
func TestHarnessUnrunnableBinaryFails(t *testing.T) {
	e := okEnv()
	e.binVersion = func(context.Context, string) (string, error) {
		return "", errors.New("exec format error")
	}

	c := checkHarness(context.Background(), validCfg(t), e)
	if c.status != fail {
		t.Errorf("unrunnable harness binary: got %v, want FAIL", c.status)
	}
	if !strings.Contains(c.detail, "not runnable") {
		t.Errorf("detail should say it is not runnable, got %q", c.detail)
	}
}

func TestHarnessReportsVersion(t *testing.T) {
	c := checkHarness(context.Background(), validCfg(t), okEnv())
	if c.status != pass {
		t.Fatalf("healthy harness: got %v, want PASS", c.status)
	}
	if !strings.Contains(c.detail, "1.2.3") {
		t.Errorf("PASS should report the version, got %q", c.detail)
	}
}

// --- backlog wiring ----------------------------------------------------------

// The four not-wired cases must stay distinguishable: two of them are survivable
// (the loop drains blind) and two are operator misconfigurations that never
// self-heal. Collapsing them would send an operator to the wrong fix.
func TestBacklogFailureModesAreDistinct(t *testing.T) {
	cases := []struct {
		name       string
		env        doctorEnv
		want       status
		wantDetail string
	}{
		{
			name:       "no creds warns and says it would run blind",
			env:        func() doctorEnv { e := okEnv(); e.apiKey = ""; return e }(),
			want:       warn,
			wantDetail: "no creds",
		},
		{
			name:       "rejected key fails",
			env:        envWithPoll(backlog.Summary{}, backlog.ErrUnauthorized),
			want:       fail,
			wantDetail: "401/403",
		},
		{
			name:       "key/route mismatch fails",
			env:        envWithPoll(backlog.Summary{}, backlog.ErrProjectRequired),
			want:       fail,
			wantDetail: "project_required",
		},
		{
			name:       "not wired warns",
			env:        envWithPoll(backlog.Summary{}, backlog.ErrNotWired),
			want:       warn,
			wantDetail: "run blind",
		},
		{
			name:       "unreachable endpoint warns",
			env:        envWithPoll(backlog.Summary{}, errors.New("dial tcp: connection refused")),
			want:       warn,
			wantDetail: "unreachable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := backlogCheck(context.Background(), "backlog", "https://clankerbar.com/api/projects/x/backlog-summary", tc.env)
			if c.status != tc.want {
				t.Errorf("got %v, want %v (detail: %q)", c.status, tc.want, c.detail)
			}
			if !strings.Contains(c.detail, tc.wantDetail) {
				t.Errorf("detail %q should mention %q", c.detail, tc.wantDetail)
			}
			if c.status != pass && c.remedy == "" {
				t.Error("a WARN/FAIL must carry a remedy line")
			}
		})
	}
}

// The project_required remedy must lead with the project SELECTOR, per the
// 2026-07-29 decision that the loop runs on the account key. Leading with "use a
// project-scoped key" would send the operator to the CI-style setup instead.
func TestProjectRequiredRemedyLeadsWithSelector(t *testing.T) {
	c := backlogCheck(context.Background(), "backlog", "https://clankerbar.com/api/backlog-summary",
		envWithPoll(backlog.Summary{}, backlog.ErrProjectRequired))

	slug := strings.Index(c.remedy, "slug")
	key := strings.Index(c.remedy, "project-scoped key")
	if slug < 0 || key < 0 {
		t.Fatalf("remedy should mention both the slug and the key fallback: %q", c.remedy)
	}
	if slug > key {
		t.Errorf("remedy should lead with the project selector, not the key: %q", c.remedy)
	}
}

func TestBacklogHealthyReportsCounts(t *testing.T) {
	c := backlogCheck(context.Background(), "backlog", "https://example.test/api/backlog-summary",
		envWithPoll(backlog.Summary{Claimable: 7, OpenQuestions: 2}, nil))

	if c.status != pass {
		t.Fatalf("healthy backlog: got %v, want PASS", c.status)
	}
	if !strings.Contains(c.detail, "7 claimable") {
		t.Errorf("PASS should report the claimable count, got %q", c.detail)
	}
}

// Nothing claimable while questions wait on the operator is the state that reads
// as a healthy queue and isn't: the loop idle-polls all night for free, and the
// operator sees a run that "found no work" rather than one they never unblocked.
func TestBacklogQuestionGatedQueueWarns(t *testing.T) {
	c := backlogCheck(context.Background(), "backlog", "https://example.test/api/backlog-summary",
		envWithPoll(backlog.Summary{Ready: 1, Claimable: 0, OpenQuestions: 2}, nil))

	if c.status != warn {
		t.Fatalf("question-gated queue: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "idle") {
		t.Errorf("detail should say the loop will idle, got %q", c.detail)
	}
	if c.remedy == "" {
		t.Error("a WARN must carry a remedy line")
	}
}

// An empty queue with no open questions is just an empty queue — warning there
// would fire on every healthy drained backlog and train the operator to skim.
func TestBacklogEmptyQueueWithoutQuestionsPasses(t *testing.T) {
	c := backlogCheck(context.Background(), "backlog", "https://example.test/api/backlog-summary",
		envWithPoll(backlog.Summary{}, nil))
	if c.status != pass {
		t.Errorf("drained queue: got %v, want PASS (%s)", c.status, c.detail)
	}
}

// A multi-project instance gets one check per project: one queue can be wired
// wrong while the others are fine, and an aggregate line would hide it.
func TestBacklogChecksEachProject(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{{Slug: "alpha"}, {Slug: "beta"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	checks := checkBacklog(context.Background(), cfg, okEnv())
	if len(checks) != 2 {
		t.Fatalf("got %d backlog checks, want one per project: %v", len(checks), names(checks))
	}
	for _, want := range []string{"backlog[alpha]", "backlog[beta]"} {
		find(t, checks, want)
	}
}

// --- config_dir --------------------------------------------------------------

func TestConfigDirUnsetWarns(t *testing.T) {
	cfg := validCfg(t)
	cfg.ConfigDir = ""

	c := checkConfigDir(cfg)
	if c.status != warn {
		t.Errorf("unset config_dir: got %v, want WARN", c.status)
	}
}

func TestConfigDirMissingFails(t *testing.T) {
	cfg := validCfg(t)
	cfg.ConfigDir = filepath.Join(t.TempDir(), "nope")

	c := checkConfigDir(cfg)
	if c.status != fail {
		t.Errorf("missing config_dir: got %v, want FAIL", c.status)
	}
}

func TestConfigDirEmptyWarns(t *testing.T) {
	cfg := validCfg(t)
	cfg.ConfigDir = t.TempDir()

	c := checkConfigDir(cfg)
	if c.status != warn {
		t.Errorf("empty config_dir: got %v, want WARN", c.status)
	}
	if !strings.Contains(c.detail, "empty") {
		t.Errorf("detail should say it is empty, got %q", c.detail)
	}
}

// A populated dir with no recognisable auth state is only a WARN: on macOS the
// credential can live in the keychain, which doctor cannot see. Claiming FAIL
// there would be asserting something we did not check.
func TestConfigDirWithoutAuthStateOnlyWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validCfg(t)
	cfg.ConfigDir = dir

	c := checkConfigDir(cfg)
	if c.status != warn {
		t.Errorf("config_dir without auth markers: got %v, want WARN", c.status)
	}
}

func TestConfigDirWithAuthStatePasses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validCfg(t)
	cfg.ConfigDir = dir

	if c := checkConfigDir(cfg); c.status != pass {
		t.Errorf("initialised config_dir: got %v, want PASS (%s)", c.status, c.detail)
	}
}

// --- state dir ---------------------------------------------------------------

// A leftover marker is the failure that looks exactly like "the backlog was
// empty": the loop stops on its first tick and exits clean.
func TestStateDirLeftoverMarkersWarn(t *testing.T) {
	for _, marker := range []string{"HALT", "STOP"} {
		t.Run(marker, func(t *testing.T) {
			cfg := validCfg(t)
			stateDir := cfg.ResolveStateDir()
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stateDir, marker), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}

			c := checkStateDir(cfg)
			if c.status != warn {
				t.Errorf("leftover %s: got %v, want WARN", marker, c.status)
			}
			if !strings.Contains(c.detail, marker) {
				t.Errorf("detail should name the %s marker, got %q", marker, c.detail)
			}
		})
	}
}

func TestStateDirCleanPasses(t *testing.T) {
	if c := checkStateDir(validCfg(t)); c.status != pass {
		t.Errorf("clean state dir: got %v, want PASS (%s)", c.status, c.detail)
	}
}

// An unwritable state dir must FAIL rather than pass on "creatable": an existing
// read-only dir is creatable (MkdirAll is a no-op) but the loop cannot log to it.
func TestStateDirUnwritableFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	cfg := validCfg(t)
	stateDir := cfg.ResolveStateDir()
	if err := os.MkdirAll(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	if c := checkStateDir(cfg); c.status != fail {
		t.Errorf("read-only state dir: got %v, want FAIL (%s)", c.status, c.detail)
	}
}

// --- session workdirs --------------------------------------------------------

// multiRepoParent builds the `~/dev` shape: a directory that is not itself a
// checkout but holds several, optionally with an agent-instructions file.
func multiRepoParent(t *testing.T, instructions string) string {
	t.Helper()
	parent := t.TempDir()
	for _, repo := range []string{"repo-a", "repo-b"} {
		if err := os.MkdirAll(filepath.Join(parent, repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if instructions != "" {
		if err := os.WriteFile(filepath.Join(parent, instructions), []byte("# orientation"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}

// Sessions spawned in a multi-repo parent with no .mcp.json get no clankerbar
// tools at all — they run, burn tokens, and cannot see the backlog.
func TestSessionMultiRepoParentWithoutMCPWarns(t *testing.T) {
	c := sessionCheck("workdir", multiRepoParent(t, "AGENTS.md"), "")
	if c.status != warn {
		t.Fatalf("multi-repo parent without .mcp.json: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, ".mcp.json") {
		t.Errorf("detail should name the missing .mcp.json, got %q", c.detail)
	}
}

// The expensive silent failure: a session started where no instruction file
// reaches it reads no protocol and no conventions, because a harness loads those
// from the cwd upward and never from the repos below it.
func TestSessionWithoutAgentInstructionsWarns(t *testing.T) {
	c := sessionCheck("workdir", multiRepoParent(t, ""), "/tmp/.mcp.json")
	if c.status != warn {
		t.Fatalf("workdir with no instruction file: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "AGENTS.md") {
		t.Errorf("detail should name the files it looked for, got %q", c.detail)
	}
	// The multi-repo remedy has to explain WHY, or an operator reasonably concludes
	// the repos' own files already cover it.
	if !strings.Contains(c.remedy, "multi-repo parent") {
		t.Errorf("remedy should explain that repos below the workdir are not loaded, got %q", c.remedy)
	}
}

// Either name satisfies it: AGENTS.md is the cross-tool convention, CLAUDE.md the
// Claude Code one, and the check is "did the operator orient these sessions",
// not "did they pick our favourite filename".
func TestSessionAcceptsEitherInstructionFile(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		t.Run(name, func(t *testing.T) {
			c := sessionCheck("workdir", multiRepoParent(t, name), "/tmp/.mcp.json")
			if c.status != pass {
				t.Fatalf("%s present: got %v, want PASS (%s)", name, c.status, c.detail)
			}
			if !strings.Contains(c.detail, name) {
				t.Errorf("PASS should name the file it found, got %q", c.detail)
			}
		})
	}
}

// A workdir that is not there kills every spawn for that project, so it is the
// one thing here that must FAIL rather than warn.
func TestSessionMissingWorkdirFails(t *testing.T) {
	c := sessionCheck("workdir[gone]", filepath.Join(t.TempDir(), "nope"), "")
	if c.status != fail {
		t.Errorf("absent workdir: got %v, want FAIL (%s)", c.status, c.detail)
	}
}

// One check per project, for the same reason the backlog check is per project: a
// multi-project instance can have one queue pointed somewhere useless while the
// rest are fine.
func TestSessionsCheckedPerProject(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{
		{Slug: "alpha", WorkDir: t.TempDir()},
		{Slug: "beta", WorkDir: t.TempDir()},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	checks := checkSessions(cfg)
	if len(checks) != 2 {
		t.Fatalf("got %d session checks, want one per project: %v", len(checks), names(checks))
	}
	for _, want := range []string{"workdir[alpha]", "workdir[beta]"} {
		find(t, checks, want)
	}
}

// A single checkout is not a multi-repo parent — one nested .git is more likely a
// vendored dependency than a workspace, and warning there would be noise on the
// most common setup of all.
func TestSingleCheckoutIsNotAMultiRepoParent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isMultiRepoParent(dir) {
		t.Error("a checkout must not be treated as a multi-repo parent")
	}
}

// --- toolchain grants --------------------------------------------------------

// seedRepo makes dir/name a checkout carrying marker.
func seedRepo(t *testing.T, dir, name, marker string) string {
	t.Helper()
	repo := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if marker != "" {
		if err := os.WriteFile(filepath.Join(repo, marker), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// toolchainCfg builds a claude config for a project named "proj" whose workdir is
// a multi-repo parent holding `proj-cli` with the given marker file, and whose
// --settings file carries the given permission rules.
func toolchainCfg(t *testing.T, marker string, allow, deny []string) *config.Config {
	t.Helper()
	parent := t.TempDir()
	seedRepo(t, parent, "proj-cli", marker)

	rules := map[string]any{"permissions": map[string]any{"allow": allow, "deny": deny}}
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(t.TempDir(), "headless.json")
	if err := os.WriteFile(settings, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Harness:      "claude",
		Prompt:       "x",
		SettingsPath: settings,
		Projects:     []config.Project{{Slug: "proj", WorkDir: parent}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The exact hole that cost one run three iterations: a Go repo in the queue, no
// `go` in the allowlist, and a headless session that fails closed — so the task
// gets written, pushed, and never compiled.
func TestToolchainWithoutGrantWarns(t *testing.T) {
	c := checkToolchains(toolchainCfg(t, "go.mod", []string{"Bash(gh:*)"}, nil))
	if c.status != warn {
		t.Fatalf("ungranted toolchain: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "go") {
		t.Errorf("detail should name the ungranted tool, got %q", c.detail)
	}
	// The remedy has to say why silence is not consent, or an operator reads a green
	// run as a verified one.
	if !strings.Contains(c.remedy, "fails closed") {
		t.Errorf("remedy should explain the fail-closed consequence, got %q", c.remedy)
	}
}

// A grant on any verb of the tool counts: the check answers "can this session
// run go at all", not "is every subcommand enumerated".
func TestToolchainGrantedByVerbPasses(t *testing.T) {
	c := checkToolchains(toolchainCfg(t, "go.mod", []string{"Bash(go build:*)", "Bash(go test:*)"}, nil))
	if c.status != pass {
		t.Fatalf("granted toolchain: got %v, want PASS (%s)", c.status, c.detail)
	}
}

// Deny wins over allow, and it has to be reported as its own state — an operator
// looking at the allow entry would otherwise call it granted and go hunting
// somewhere else for the refusal.
func TestToolchainDeniedIsDistinctFromMissing(t *testing.T) {
	c := checkToolchains(toolchainCfg(t, "go.mod", []string{"Bash(go:*)"}, []string{"Bash(go:*)"}))
	if c.status != warn {
		t.Fatalf("denied toolchain: got %v, want WARN (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "denied") {
		t.Errorf("detail should say the tool is denied, not merely ungranted, got %q", c.detail)
	}
}

// Claude MERGES the --settings file with the config dir's own settings, so
// auditing only the drain policy would report a tool as ungranted when the
// operator's own settings already allow it.
func TestToolchainGrantFromConfigDirCounts(t *testing.T) {
	cfg := toolchainCfg(t, "go.mod", nil, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"permissions":{"allow":["Bash(go:*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigDir = dir

	if c := checkToolchains(cfg); c.status != pass {
		t.Errorf("grant in the config dir should count: got %v (%s)", c.status, c.detail)
	}
}

// A bare package.json cannot name its package manager, and guessing would warn
// about npm on every pnpm repo in existence. The lockfile is the marker.
func TestToolchainAmbiguousMarkerIsNotGuessed(t *testing.T) {
	c := checkToolchains(toolchainCfg(t, "package.json", nil, nil))
	if c.status != pass {
		t.Fatalf("ambiguous marker: got %v, want PASS (%s)", c.status, c.detail)
	}
	if !strings.Contains(c.detail, "no unambiguous") {
		t.Errorf("detail should say nothing was detected, got %q", c.detail)
	}
}

// Scope discipline: a session rooted in a multi-repo parent sits above every
// checkout on the machine, and warning about each one's toolchain would bury the
// grant that actually blocks the backlog. Only the project's own repos count.
func TestToolchainIgnoresUnrelatedSiblingRepos(t *testing.T) {
	cfg := toolchainCfg(t, "go.mod", []string{"Bash(go:*)"}, nil)
	parent := cfg.Projects[0].WorkDir
	seedRepo(t, parent, "someone-elses-app", "yarn.lock")

	c := checkToolchains(cfg)
	if c.status != pass {
		t.Fatalf("unrelated sibling repo: got %v, want PASS (%s)", c.status, c.detail)
	}
	if strings.Contains(c.detail, "yarn") {
		t.Errorf("a repo outside the project must not be reported, got %q", c.detail)
	}
}

func TestBashHeadParsesRuleForms(t *testing.T) {
	for rule, want := range map[string]string{
		"Bash(go build:*)":     "go",
		"Bash(go:*)":           "go",
		"Bash(go version)":     "go",
		" Bash(pnpm test:*) ":  "pnpm",
		"WebFetch(domain:x)":   "",
		"Edit(//tmp/**)":       "",
		"mcp__clankerbar__foo": "",
	} {
		if got := bashHead(rule); got != want {
			t.Errorf("bashHead(%q) = %q, want %q", rule, got, want)
		}
	}
}

// --- permission policy -------------------------------------------------------

func TestPermissionsClaudeWithoutSettingsWarns(t *testing.T) {
	cfg := validCfg(t)
	cfg.SettingsPath = ""

	if c := checkPermissions(cfg); c.status != warn {
		t.Errorf("claude without settings_path: got %v, want WARN", c.status)
	}
}

func TestPermissionsClaudeUnparseableSettingsFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validCfg(t)
	cfg.SettingsPath = path

	if c := checkPermissions(cfg); c.status != fail {
		t.Errorf("unparseable settings: got %v, want FAIL", c.status)
	}
}

func TestPermissionsClaudeValidSettingsPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"deny":["WebFetch"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validCfg(t)
	cfg.SettingsPath = path

	if c := checkPermissions(cfg); c.status != pass {
		t.Errorf("valid settings: got %v, want PASS (%s)", c.status, c.detail)
	}
}

// exec takes the LAST duplicate env key, so an operator's own OPENCODE_PERMISSION
// silently replaces the adapter's fail-closed policy.
func TestPermissionsOpencodeEnvOverrideWarns(t *testing.T) {
	cfg := &config.Config{
		Harness: "opencode",
		Prompt:  "x",
		WorkDir: t.TempDir(),
		Env:     map[string]string{"OPENCODE_PERMISSION": `{"bash":"allow"}`},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if c := checkPermissions(cfg); c.status != warn {
		t.Errorf("opencode permission override: got %v, want WARN", c.status)
	}
}

// --- budget ------------------------------------------------------------------

func TestBudgetNegativeValuesFail(t *testing.T) {
	cfg := validCfg(t)
	cfg.Budget = config.Budget{MaxTokens: -1}

	c := checkBudget(cfg)
	if c.status != fail {
		t.Errorf("negative budget: got %v, want FAIL", c.status)
	}
	if !strings.Contains(c.detail, "max_tokens") {
		t.Errorf("detail should name the offending field, got %q", c.detail)
	}
}

// No ceiling is a legitimate daemon setup — informational, never a failure.
func TestBudgetUnsetIsInformational(t *testing.T) {
	c := checkBudget(validCfg(t))
	if c.status != pass {
		t.Errorf("no budget: got %v, want PASS", c.status)
	}
	if !strings.Contains(c.detail, "no ceiling") {
		t.Errorf("detail should note the absent ceiling, got %q", c.detail)
	}
}

func TestBudgetSetIsReported(t *testing.T) {
	cfg := validCfg(t)
	cfg.Budget = config.Budget{MaxTokens: 500000}

	c := checkBudget(cfg)
	if c.status != pass || !strings.Contains(c.detail, "max_tokens=500000") {
		t.Errorf("set budget: got %v %q", c.status, c.detail)
	}
}

// --- end to end --------------------------------------------------------------

// The exit-code contract: any FAIL makes the command exit non-zero, so
// `clankerbar doctor && clankerbar run` gates a cron wrapper.
func TestDoctorRunExitsNonZeroOnFail(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "clankerbar.json")
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"claude","workdir":"`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	e := okEnv()
	e.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	var out strings.Builder
	err := doctorRun(context.Background(), &out, cfgPath, config.Overrides{}, e)
	if err == nil {
		t.Fatal("doctorRun returned nil despite a FAILing check; the command would exit 0")
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("output should carry a FAIL line:\n%s", out.String())
	}
}

func TestDoctorRunSucceedsWhenOnlyWarnings(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "clankerbar.json")
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"claude","workdir":"`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	// config_dir is unset and settings_path is unset: both WARN, neither FAILs.
	if err := doctorRun(context.Background(), &out, cfgPath, config.Overrides{}, okEnv()); err != nil {
		t.Fatalf("warnings must not fail the command: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("expected warnings in output:\n%s", out.String())
	}
}

// An unloadable config short-circuits: every later check would be describing a
// config the loop would never accept.
func TestDoctorRunFailsOnUnparseableConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "clankerbar.json")
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := doctorRun(context.Background(), &out, cfgPath, config.Overrides{}, okEnv()); err == nil {
		t.Fatal("an unparseable config must fail the command")
	}
	if strings.Count(out.String(), "\n") > 3 {
		t.Errorf("should stop at the config check, got:\n%s", out.String())
	}
}

// Every check must be present and every WARN/FAIL must carry a remedy — the
// done-condition is one status line per check plus a remedy where it matters.
func TestEveryCheckIsReportedWithARemedy(t *testing.T) {
	checks := doctorChecks(context.Background(), validCfg(t), okEnv())

	for _, want := range []string{
		"config", "harness", "config_dir", "backlog",
		"state_dir", "workdir", "permissions", "toolchains", "budget",
	} {
		c := find(t, checks, want)
		if c.detail == "" {
			t.Errorf("check %q has no detail line", want)
		}
		if c.status != pass && c.remedy == "" {
			t.Errorf("check %q is %v but carries no remedy", want, c.status)
		}
	}
}
