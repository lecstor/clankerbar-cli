package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runOptions is one rig run, fully resolved from flags and the environment.
type runOptions struct {
	Scenario        string
	Model           string
	Provider        string
	Upstream        string
	MCPConfig       string
	ConfigDir       string
	Workdir         string
	Out             string
	Schemas         string
	PlaneURL        string
	SchemaTool      string
	PlaneKeyFile    string
	ProviderKeyFile string
	ProviderKeyEnv  string
	ExtraEnv        []string
	Deadline        time.Duration
	MaxToolCalls    int
	Prompt          string
	OpencodeBin     string
	DryRun          bool
	Keep            bool
	StartedAt       time.Time
	// ServerURL and ServerPassword are set after the MCP preflight: the run
	// attaches to the private server the rig started, never to a second one of
	// its own (that is the race the preflight exists to close).
	ServerURL      string
	ServerPassword string
}

// runRecord is the run.json artifact: everything needed to understand the run
// without this session's memory, plus the outcome.
type runRecord struct {
	RunID          string   `json:"runId"`
	Scenario       string   `json:"scenario"`
	Model          string   `json:"model"`
	Provider       string   `json:"provider"`
	Upstream       string   `json:"upstream"`
	ProxyURL       string   `json:"proxyUrl"`
	BaseConfig     string   `json:"baseConfig,omitempty"`
	SourceConfig   string   `json:"sourceConfig"`
	ContentConfig  string   `json:"contentConfig"`
	Workdir        string   `json:"workdir"`
	Out            string   `json:"out"`
	SchemaSource   string   `json:"schemaSource"`
	SchemaFile     string   `json:"schemaFile"`
	Tools          []string `json:"tools"`
	Deadline       string   `json:"deadline"`
	MaxToolCalls   int      `json:"maxToolCalls"`
	StartedAt      string   `json:"startedAt"`
	FinishedAt     string   `json:"finishedAt"`
	DurationMS     int64    `json:"durationMs"`
	ExitCode       int      `json:"exitCode"`
	TimedOut       bool     `json:"timedOut"`
	Capped         bool     `json:"capped"`
	StubStarted    bool     `json:"stubStarted"`
	ProviderReqs   int      `json:"providerRequests"`
	Prompt         string   `json:"prompt"`
	OpencodeBin    string   `json:"opencodeBin"`
	IsolationDirs  []string `json:"isolationDirs"`
	OpencodeConfig string   `json:"opencodeConfigDir,omitempty"`
}

// defaultBaseConfig is the fleet's own opencode MCP config. When it exists the
// rig starts from it, so the session's config is the fleet's; when it does not,
// the rig runs with the stub as the only MCP server.
func defaultBaseConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".config", "clankerbar", "opencode-mcp.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func runCmd(args []string) int {
	fs := flagSet("run")
	var o runOptions
	fs.StringVar(&o.Scenario, "scenario", "padding", "scenario: padding, reasoning-replay or schema")
	fs.StringVar(&o.Model, "model", "", "provider/model, e.g. commandcode/deepseek/deepseek-v4.1-flash (required)")
	fs.StringVar(&o.Provider, "provider", "", "provider id (default: parsed from --model)")
	fs.StringVar(&o.Upstream, "upstream", "", "real provider base URL (default: options.baseURL in --mcp-config)")
	fs.StringVar(&o.MCPConfig, "mcp-config", "", "source config (default ~/.config/clankerbar/opencode-mcp.json if present)")
	fs.StringVar(&o.ConfigDir, "config-dir", "", "OPENCODE_CONFIG_DIR to pass through (default: the environment's)")
	fs.StringVar(&o.Workdir, "workdir", "", "session sandbox (default <out>/sandbox)")
	fs.StringVar(&o.Out, "out", "", "run directory (default $TMPDIR/opencode-rig/<scenario>-<stamp>)")
	fs.StringVar(&o.Schemas, "schemas", "live", "tool catalog: live, or a file")
	fs.StringVar(&o.PlaneURL, "plane-url", "https://clankerbar.com/mcp/clankerbar", "plane MCP URL for the live schema fetch")
	fs.StringVar(&o.SchemaTool, "schema-tool", defaultSchemaTool, "tool the schema scenario calls, as the catalog names it")
	fs.StringVar(&o.PlaneKeyFile, "plane-key-file", "", "read CLANKERBAR_API_KEY from this file")
	fs.StringVar(&o.ProviderKeyFile, "provider-key-file", "", "read the provider key from this file")
	fs.StringVar(&o.ProviderKeyEnv, "provider-key-env", "COMMANDCODE_API_KEY", "environment variable the provider key is set in")
	fs.Var((*stringList)(&o.ExtraEnv), "env", "extra KEY=VALUE for the session (repeatable)")
	fs.DurationVar(&o.Deadline, "deadline", 5*time.Minute, "kill the session after this")
	fs.IntVar(&o.MaxToolCalls, "max-tool-calls", 6, "the stub refuses the Nth call and the run stops there (0 = no cap)")
	fs.StringVar(&o.Prompt, "prompt", "", "override the scenario prompt")
	fs.StringVar(&o.OpencodeBin, "opencode-bin", "opencode", "opencode binary")
	fs.BoolVar(&o.DryRun, "dry-run", false, "resolve and print everything, spawn nothing")
	fs.BoolVar(&o.Keep, "keep", false, "accepted for symmetry; runs are always kept")
	if err := parseFlags(fs, args); err != nil {
		return 2
	}
	code, err := runRig(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: %v\n", err)
		return 1
	}
	return code
}

// stringList collects repeated KEY=VALUE flags.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("--env wants KEY=VALUE, got %q", v)
	}
	*s = append(*s, v)
	return nil
}

func runRig(o runOptions) (int, error) {
	o.StartedAt = time.Now()
	if _, ok := scenarios()[o.Scenario]; !ok {
		return 1, fmt.Errorf("unknown scenario %q (have padding, reasoning-replay, schema)", o.Scenario)
	}
	if o.Model == "" && o.Provider == "" {
		return 1, fmt.Errorf("--model is required: the rig must know which provider's baseURL to redirect and the report must name the model asked for")
	}
	if o.Provider == "" {
		p, _, err := providerFromModel(o.Model)
		if err != nil {
			return 1, err
		}
		o.Provider = p
	}
	if o.Deadline <= 0 {
		return 1, fmt.Errorf("--deadline must be positive")
	}
	if o.ConfigDir != "" {
		o.ConfigDir = expandHome(o.ConfigDir)
	}

	// Run directory. Everything the run produces lives under it, so a report can
	// always be tied to its raw logs.
	if o.Out == "" {
		o.Out = filepath.Join(os.TempDir(), "opencode-rig", runID(o.Scenario, o.StartedAt))
	}
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return 1, err
	}
	o.Out = out
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return 1, err
	}
	// One run per directory. Reusing a directory would mix this run's logs with
	// a previous run's and let a stale stub-capped marker stop a healthy
	// session, so refuse rather than silently contaminate the evidence.
	if fileExists(filepath.Join(o.Out, "run.json")) {
		return 1, fmt.Errorf("run directory %s already holds a run (run.json); pass a new --out", o.Out)
	}

	// The sandbox: an empty directory, deliberately not the repo. The rig studies
	// the tool surface and the provider wire, not the checkout, and the permission
	// policy scoped to it means a curious model cannot read the real repo.
	if o.Workdir == "" {
		o.Workdir = filepath.Join(o.Out, "sandbox")
	}
	if err := os.MkdirAll(o.Workdir, 0o755); err != nil {
		return 1, err
	}
	o.Workdir, err = filepath.Abs(o.Workdir)
	if err != nil {
		return 1, err
	}

	// Tool catalog: live from the plane, or a file (the schema scenario's
	// proposed shape).
	schemasPath := filepath.Join(o.Out, "schemas.json")
	var cat catalog
	if o.Schemas == "live" {
		key := os.Getenv("CLANKERBAR_API_KEY")
		if o.PlaneKeyFile != "" {
			key, err = readKeyFile(o.PlaneKeyFile)
			if err != nil {
				return 1, err
			}
		}
		cat, err = fetchCatalog(context.Background(), o.PlaneURL, key)
		if err != nil {
			return 1, fmt.Errorf("fetching the plane's tool schemas: %w", err)
		}
		cat.Source = o.PlaneURL
		if err := saveCatalog(schemasPath, cat); err != nil {
			return 1, err
		}
	} else {
		cat, err = loadCatalog(expandHome(o.Schemas))
		if err != nil {
			return 1, err
		}
		if cat.Source == "" {
			cat.Source = o.Schemas
		}
		// Keep the exact catalog the run served, beside the rest of the evidence.
		if err := saveCatalog(schemasPath, cat); err != nil {
			return 1, err
		}
	}
	if o.Scenario == "schema" && !contains(cat.toolNames(), o.SchemaTool) {
		return 1, fmt.Errorf("schema scenario: tool %q is not in the catalog (have %s)", o.SchemaTool, strings.Join(cat.toolNames(), ", "))
	}

	// Base config: the fleet's own file when present.
	basePath := o.MCPConfig
	if basePath == "" {
		basePath = defaultBaseConfig()
	}
	if basePath != "" {
		basePath = expandHome(basePath)
		if _, err := os.Stat(basePath); err != nil {
			return 1, fmt.Errorf("--mcp-config: %w", err)
		}
	}

	// Decide the upstream from the base config before anything is rewritten.
	upstream := o.Upstream
	if upstream == "" {
		upstream, err = providerBaseURL(basePath, o.Provider)
		if err != nil {
			return 1, err
		}
	}

	// The proxy binds port 0 here, before the session exists: a free port chosen
	// by the kernel and a listener that is already accepting. There is no
	// readiness sleep to get wrong.
	ln, proxyURL, err := listen()
	if err != nil {
		return 1, fmt.Errorf("cannot bind the proxy's port: %w", err)
	}
	defer ln.Close()

	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("locating the rig binary (needed to spawn the stub): %w", err)
	}
	stubArgs := []string{
		"stub",
		"--scenario", o.Scenario,
		"--out", o.Out,
		"--schemas", schemasPath,
		"--max-calls", strconv.Itoa(o.MaxToolCalls),
	}
	srcData, replacedUpstream, err := buildSourceConfig(sourceConfigOpts{
		BasePath:     basePath,
		ProviderID:   o.Provider,
		ProxyBaseURL: proxyURL,
		Self:         self,
		StubArgs:     stubArgs,
	})
	if err != nil {
		return 1, err
	}
	if o.Upstream == "" && replacedUpstream != "" {
		fmt.Fprintf(os.Stderr, "opencode-rig: provider %s baseURL %s -> %s (proxy)\n", o.Provider, replacedUpstream, proxyURL)
	}
	sourcePath, err := writeSourceConfig(o.Out, srcData)
	if err != nil {
		return 1, err
	}
	// readOnly=true pins the adapter's READ-ONLY policy (internal/harness:
	// shell and edit denied, reads scoped to the workdir). The rig never studies
	// a write, and the fleet's writable policy allows `shell` unscoped - a shell
	// tool would read the whole checkout regardless of the sandbox's path rules.
	content, err := generatedContent(sourcePath, o.Workdir, true)
	if err != nil {
		return 1, fmt.Errorf("generating the session config through harness.OpencodeSessionConfig: %w", err)
	}
	contentPath := filepath.Join(o.Out, "opencode-config-content.json")
	// The content layer carries the provider block, so a literal apiKey in the
	// source config lands here too: never world-readable.
	if err := writeCredentialFile(contentPath, content); err != nil {
		return 1, err
	}

	scenario := scenarios()[o.Scenario]
	prompt := scenario.promptFor(o.SchemaTool)
	if o.Prompt != "" {
		prompt = o.Prompt
	}
	o.Prompt = prompt

	isolationPairs := [][2]string{
		{"XDG_DATA_HOME", filepath.Join(o.Out, "xdg", "data")},
		{"XDG_STATE_HOME", filepath.Join(o.Out, "xdg", "state")},
		{"XDG_CACHE_HOME", filepath.Join(o.Out, "xdg", "cache")},
	}
	var isolation []string
	for _, pair := range isolationPairs {
		if err := os.MkdirAll(pair[1], 0o755); err != nil {
			return 1, err
		}
		isolation = append(isolation, pair[0]+"="+pair[1])
	}

	upstreamURL, err := url.Parse(upstream)
	if err != nil || upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		return 1, fmt.Errorf("--upstream (or the config's options.baseURL) is not an absolute URL: %q", upstream)
	}
	proxy, err := newRecordingProxy(upstreamURL, o.Out)
	if err != nil {
		return 1, err
	}

	rec := runRecord{
		RunID:          filepath.Base(o.Out),
		Scenario:       o.Scenario,
		Model:          o.Model,
		Provider:       o.Provider,
		Upstream:       upstream,
		ProxyURL:       proxyURL,
		BaseConfig:     basePath,
		SourceConfig:   sourcePath,
		ContentConfig:  contentPath,
		Workdir:        o.Workdir,
		Out:            o.Out,
		SchemaSource:   cat.Source,
		SchemaFile:     schemasPath,
		Tools:          cat.toolNames(),
		Deadline:       o.Deadline.String(),
		MaxToolCalls:   o.MaxToolCalls,
		StartedAt:      o.StartedAt.UTC().Format(time.RFC3339),
		Prompt:         prompt,
		OpencodeBin:    o.OpencodeBin,
		IsolationDirs:  isolation,
		OpencodeConfig: o.ConfigDir,
	}
	fmt.Printf("opencode-rig: %s\n  scenario: %s\n  model: %s (provider %s)\n  tools: %d from %s\n  proxy: %s -> %s\n  run dir: %s\n",
		rec.RunID, o.Scenario, o.Model, o.Provider, len(cat.Tools), schemaSourceLabel(o.Schemas), proxyURL, upstream, o.Out)

	if o.DryRun {
		// Deliberately no run.json: a dry run spawns nothing, and the reuse guard
		// above refuses any directory holding one - so writing the record here
		// would make the very directory unusable for the real run it exists to
		// prepare.
		display := o
		display.ServerURL = "<url>"
		fmt.Printf("  dry run: would start opencode serve, wait for MCP server %q, then attach with:\n  %s %s\n",
			"clankerbar", o.OpencodeBin, strings.Join(opencodeArgs(display), " "))
		fmt.Printf("  session config content: %s\n", contentPath)
		return 0, nil
	}

	// Start the proxy before the session: it is already listening, and this only
	// puts the handler on the listener. A listener that fails now cannot be
	// recovered from, but the session will fail loudly on the first request.
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		_ = srv.Close()
	}()

	// Provider key, if the caller keeps it in a file (the fleet does:
	// @~/.config/clankerbar/commandcode-key).
	extraEnv := append([]string{}, o.ExtraEnv...)
	if o.ProviderKeyFile != "" {
		key, err := readKeyFile(o.ProviderKeyFile)
		if err != nil {
			return 1, err
		}
		extraEnv = append(extraEnv, o.ProviderKeyEnv+"="+key)
	}

	// CLA-703's preflight, mirrored: start the private server, wait until the
	// stub's MCP server reports connected, and only then attach the run. Without
	// this the model's first turn can be offered no clankerbar tools at all -
	// verified, not assumed: the first live run of this rig did exactly that.
	serverEnv := sessionEnv(o, content, isolation, extraEnv)
	fmt.Printf("  starting opencode serve; waiting for MCP server %q to connect...\n", "clankerbar")
	sessionServer, err := startRigServer(o.OpencodeBin, serverEnv, o.Workdir, o.Out)
	if err != nil {
		return 1, err
	}
	defer sessionServer.stop()
	if err := sessionServer.waitForMCP(context.Background(), "clankerbar", o.Workdir, rigMCPTimeout); err != nil {
		return 1, fmt.Errorf("MCP preflight: %w", err)
	}
	o.ServerURL, o.ServerPassword = sessionServer.url, sessionServer.password

	code, err := runSession(o, content, isolation, extraEnv)
	if err != nil {
		return 1, err
	}

	// The raw event stream decides the report's step table; the stub's log and
	// the captured requests are read from disk so the report never depends on
	// what happened to be in memory.
	events, eventErr := readEvents(filepath.Join(o.Out, "opencode-events.jsonl"))
	if eventErr != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: reading events: %v\n", eventErr)
	}
	calls, callErr := readStubCalls(filepath.Join(o.Out, "stub-calls.jsonl"))
	if callErr != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: reading stub calls: %v\n", callErr)
	}
	requests, reqErr := readRequests(filepath.Join(o.Out, "requests"))
	if reqErr != nil {
		fmt.Fprintf(os.Stderr, "opencode-rig: reading requests: %v\n", reqErr)
	}

	rec.ProviderReqs = len(requests)
	rec.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	rec.DurationMS = time.Since(o.StartedAt).Milliseconds()
	rec.ExitCode = code
	rec.Capped = fileExists(filepath.Join(o.Out, "stub-capped"))
	rec.StubStarted = fileExists(filepath.Join(o.Out, "stub-started"))
	rec.TimedOut = fileExists(filepath.Join(o.Out, "timed-out"))
	if err := writeJSON(filepath.Join(o.Out, "run.json"), rec); err != nil {
		return 1, err
	}

	reportPath := filepath.Join(o.Out, "report.md")
	if err := writeReport(reportPath, rec, events, calls, requests); err != nil {
		return 1, err
	}
	fmt.Printf("\nreport: %s\n%s\n", reportPath, renderReport(rec, events, calls, requests))
	return code, nil
}

// runSession spawns opencode and enforces the two bounds: the deadline and the
// stub's call cap. Both end the WHOLE process group, so a stuck child of
// opencode cannot outlive the run.
func runSession(o runOptions, content []byte, isolation, extraEnv []string) (int, error) {
	args := opencodeArgs(o)
	cmd := exec.Command(o.OpencodeBin, args...)
	cmd.Dir = o.Workdir

	// stdin from /dev/null. This is not defensive: `opencode run` with a stdin
	// that is neither a TTY nor closed waits forever, printing "cli starting"
	// and nothing else. Leaving cmd.Stdin nil also gives os.DevNull, but opening
	// it explicitly makes the guarantee visible and testable.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return 1, err
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	eventsPath := filepath.Join(o.Out, "opencode-events.jsonl")
	stderrPath := filepath.Join(o.Out, "opencode-stderr.log")
	eventsFile, err := os.Create(eventsPath)
	if err != nil {
		return 1, err
	}
	defer eventsFile.Close()
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		return 1, err
	}
	defer stderrFile.Close()
	cmd.Stdout = eventsFile
	cmd.Stderr = stderrFile

	// The session's environment: the fleet's config layers, isolated data dirs,
	// and the same $PWD pin the adapter applies (opencode reads $PWD, not just
	// the real cwd, to decide its instance directory).
	env := sessionEnv(o, content, isolation, extraEnv)
	cmd.Env = env
	configureProcessGroup(cmd)

	fmt.Printf("  session: %s %s\n", o.OpencodeBin, strings.Join(args, " "))
	fmt.Printf("  waiting for the session (deadline %s, cap %d calls)...\n", o.Deadline, o.MaxToolCalls)

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting %s: %w", o.OpencodeBin, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.NewTimer(o.Deadline)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	cappedPath := filepath.Join(o.Out, "stub-capped")

	killedFor := ""
	wait := func(grace time.Duration) error {
		select {
		case err := <-done:
			return err
		case <-time.After(grace):
			forceKillProcessGroup(cmd)
			return <-done
		}
	}

	var waitErr error
loop:
	for {
		select {
		case waitErr = <-done:
			break loop
		case <-deadline.C:
			killedFor = "deadline"
			killProcessGroup(cmd)
			waitErr = wait(10 * time.Second)
			break loop
		case <-ticker.C:
			if fileExists(cappedPath) {
				killedFor = "cap"
				killProcessGroup(cmd)
				waitErr = wait(10 * time.Second)
				break loop
			}
		}
	}

	if killedFor == "deadline" {
		_ = os.WriteFile(filepath.Join(o.Out, "timed-out"), []byte(o.Deadline.String()+"\n"), 0o644)
		fmt.Fprintf(os.Stderr, "opencode-rig: killed the session at the %s deadline\n", o.Deadline)
	}
	if killedFor == "cap" {
		fmt.Fprintf(os.Stderr, "opencode-rig: stopped the session at the %d-call cap\n", o.MaxToolCalls)
	}

	code := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
			if code < 0 {
				code = 1
			}
		} else {
			code = 1
		}
	}
	if killedFor == "deadline" {
		// 124 is the conventional "timed out" code (GNU timeout).
		return 124, nil
	}
	return code, nil
}

// opencodeArgs is the fleet's argv shape, pinned here so the rig's session is
// invoked the way the adapter invokes one. --print-logs sends opencode's own
// log to stderr, which is captured per run: the shared log file named in the
// task's traps is then never read at all.
func opencodeArgs(o runOptions) []string {
	args := []string{
		"--print-logs",
		"run", "--format", "json", "--agent", "build",
	}
	if o.ServerURL != "" {
		// Attach to the server whose MCP connection the preflight proved. No
		// --standalone: that asks the CLI for a SECOND server, reintroducing the
		// race against the one that was just verified.
		args = append(args, "--server", o.ServerURL)
	} else {
		args = append(args, "--standalone")
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return append(args, "--", o.Prompt)
}

// sessionEnv builds the child's environment. Later entries win in exec (a
// duplicate key is last-one-wins), but existing entries are replaced rather
// than duplicated so the log of the environment is unambiguous.
func sessionEnv(o runOptions, content []byte, isolation, extra []string) []string {
	overrides := []string{
		"PWD=" + o.Workdir,
		"OPENCODE_CONFIG=" + filepath.Join(o.Out, "opencode-config.json"),
		"OPENCODE_CONFIG_CONTENT=" + string(content),
	}
	if o.ConfigDir != "" {
		overrides = append(overrides, "OPENCODE_CONFIG_DIR="+o.ConfigDir)
	}
	overrides = append(overrides, isolation...)
	// A caller-supplied OPENCODE_PERMISSION is a 1.x relic and must not shadow
	// the content layer's policy. OPENCODE_PASSWORD is cleared here so an
	// inherited value cannot reach the server; the run adds its own below.
	// CLANKERBAR_API_KEY is cleared too: the session talks to the stub and has no
	// plane connection, so the key is only an exposure - an ambient MCP server
	// loaded from another config layer would otherwise inherit it.
	overrides = append(overrides, "OPENCODE_PERMISSION=", "OPENCODE_PASSWORD=", "CLANKERBAR_API_KEY=")
	if o.ServerPassword != "" {
		overrides = append(overrides, "OPENCODE_PASSWORD="+o.ServerPassword)
	}
	overrides = append(overrides, extra...)
	return mergeEnv(os.Environ(), overrides)
}

// mergeEnv replaces keys named in overrides and appends new ones. A key that
// appears more than once in base keeps its LAST value (exec's rule), and the
// output holds one entry per key in first-appearance order.
func mergeEnv(base, overrides []string) []string {
	keyOf := func(s string) string {
		if i := strings.Index(s, "="); i >= 0 {
			return s[:i]
		}
		return s
	}
	var order []string
	values := map[string]string{}
	for _, s := range append(append([]string{}, base...), overrides...) {
		k := keyOf(s)
		if _, seen := values[k]; !seen {
			order = append(order, k)
		}
		values[k] = s
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, values[k])
	}
	return out
}

// providerBaseURL reads provider.<id>.options.baseURL from the base config.
func providerBaseURL(path, providerID string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("provider %s declares no baseURL and there is no --mcp-config; pass --upstream", providerID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if p, ok := doc.Provider[providerID]; ok && p.Options.BaseURL != "" {
		return p.Options.BaseURL, nil
	}
	return "", fmt.Errorf("provider %s declares no options.baseURL in %s; pass --upstream <real provider base URL>", providerID, path)
}

func readKeyFile(path string) (string, error) {
	b, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return key, nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyFile is used to keep a copy of an external catalog in the run directory.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
