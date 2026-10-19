package harness

// opencode_mcp.go — the MCP preflight for opencode sessions (CLA-703).
//
// opencode 2.x connects MCP servers asynchronously, while `opencode run`
// submits the first model turn as soon as it has a session. On 2026-10-19 that
// race cost half a review panel: two members were offered no clankerbar tools
// on their first turn, decided they could not review, and ended in seconds —
// and the driver, reading a clean exit that recorded nothing, had no way to
// tell that from a reviewer that did the work badly.
//
// The fix is to remove the race rather than describe it: the adapter starts its
// OWN server (`opencode serve`) with the same pinned config, asks that server's
// `/api/mcp` endpoint until the clankerbar server reports `connected`, and only
// THEN attaches the run to it with `--server`. The model's first turn is
// therefore the first turn that can use the tools. If the server never reports
// connected, the session fails with ErrMCPNotConnected — a named infrastructure
// error the driver retries — instead of producing a silent "no record".
//
// The server API is opencode 2.x's own, verified against 2.0.32:
//
//	GET /api/mcp?location[directory]=<dir>
//	{"location":{"directory":"..."},"data":[
//	  {"name":"clankerbar","status":{"status":"connected"}},
//	  {"name":"other","status":{"status":"pending"}}]}
//
// Statuses are connected | pending | disabled | failed | needs_auth. `disabled`
// is an answer (there is nothing to wait for); `failed` and `needs_auth` are a
// verdict, not a race, so they fail immediately with the status in the message.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/jsonc"
)

// opencodeMCPStartupTimeout bounds each preflight stage — the server announcing
// itself, and then the clankerbar server reporting connected — and is a var so
// tests can shrink it. Thirty seconds is six times opencode's own default MCP
// startup timeout (5s), which leaves room for a remote server's handshake and
// one opencode-side retry without making a broken MCP config look like a hang.
var opencodeMCPStartupTimeout = 30 * time.Second

// opencodeMCPPollInterval is how often the status endpoint is polled. A var for
// the same reason; 100ms is frequent enough to keep the added latency small and
// cheap enough that a 30-second wait is 300 local GETs, not a session.
var opencodeMCPPollInterval = 100 * time.Millisecond

// opencodeServer is a private `opencode serve` process the adapter owns for the
// duration of one run. `url` and `password` are what a run needs to attach.
type opencodeServer struct {
	url      string
	password string
	cmd      *exec.Cmd

	// exited closes when cmd.Wait returns. Wait is called in ONE goroutine, so
	// stop() never double-waits; it kills the group and waits for this channel.
	exited chan struct{}
	stderr *tail
}

// opencodeWorkDir canonicalises a session's directory the way the SERVER will
// key its location: absolute, with symlinks resolved.
//
// This is load-bearing, not tidiness. On macOS a temp dir is `/var/...`, a
// symlink to `/private/var/...`; the run's session is recorded at the resolved
// path, while an `/api/mcp?location[directory]=/var/...` query makes the server
// create a SECOND location keyed by the raw path. Polling that one reports
// `connected` for a location no session will ever use, the real location's MCP
// state finishes connecting later, and the first model turn still races — the
// exact failure this file exists to remove (verified against 2.0.32).
//
// EvalSymlinks needs the path to exist; a path it cannot resolve falls back to
// the absolute form, which is the best key available and still matches what the
// CLI records for an ordinary directory.
func opencodeWorkDir(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// startOpencodeServer launches `opencode serve` on a free loopback port and
// reads the address and password it announces. The process is put in its own
// group, so stopping it also reaps any MCP child it spawned.
func startOpencodeServer(ctx context.Context, env []string, dir string) (*opencodeServer, error) {
	cmd := exec.CommandContext(ctx, "opencode", "serve", "--port", "0", "--hostname", "127.0.0.1")
	setupProcessGroup(cmd)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	// The default Cancel kills only the direct child; a private server is a
	// parent to its MCP processes, so a wall-clock kill must take the group.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	// A descendant that escaped the group can hold the pipes open past the kill;
	// WaitDelay is the backstop that lets Wait return anyway.
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opencode serve: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("opencode serve: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: starting opencode serve: %v", ErrMCPNotConnected, err)
	}
	s := &opencodeServer{cmd: cmd, exited: make(chan struct{}), stderr: newTail()}
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()
	// Drain stderr for the diagnostic, bounded like every other stream tail.
	go func() { _, _ = io.Copy(s.stderr, stderr) }()
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			default:
				// Nobody is reading any more (the preflight is over); keep
				// draining so the server cannot block on a full stdout pipe.
			}
		}
	}()

	timer := time.NewTimer(opencodeMCPStartupTimeout)
	defer timer.Stop()
	for s.url == "" || s.password == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				s.stop()
				return nil, fmt.Errorf("%w: opencode serve exited before it announced its address and password%s",
					ErrMCPNotConnected, serverDiagnostic(s.stderr))
			}
			if v, found := strings.CutPrefix(line, "server listening on "); found {
				s.url = strings.TrimSpace(v)
			}
			if v, found := strings.CutPrefix(line, "server password "); found {
				s.password = strings.TrimSpace(v)
			}
		case <-timer.C:
			s.stop()
			return nil, fmt.Errorf("%w: opencode serve did not announce its address and password within %s",
				ErrMCPNotConnected, opencodeMCPStartupTimeout)
		case <-ctx.Done():
			s.stop()
			return nil, ctx.Err()
		}
	}
	return s, nil
}

// stop kills the server's process group and waits for its reaper. Safe on nil.
func (s *opencodeServer) stop() {
	if s == nil || s.cmd == nil {
		return
	}
	killProcessGroup(s.cmd)
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		// WaitDelay already force-closes the pipes; this is the last resort so a
		// wedged reaper cannot hang the driver on teardown.
	}
}

// waitForMCP polls the private server until every wanted MCP server is
// connected (or disabled), and fails with ErrMCPNotConnected if one cannot be.
//
// A missing entry is treated as pending, not as success: the server loads
// config per location, and the project layer may not have arrived on the first
// poll. `failed`/`needs_auth` fail at once — those are answers, and waiting the
// full timeout on them only delays the retry the driver owes.
func (s *opencodeServer) waitForMCP(ctx context.Context, names []string, dir string) error {
	if len(names) == 0 {
		return nil
	}
	deadline := time.Now().Add(opencodeMCPStartupTimeout)
	last := map[string]string{}
	var lastErr error
	for {
		states, err := s.mcpStates(ctx, dir)
		if err == nil {
			pending := false
			for _, name := range names {
				state, found := states[name]
				last[name] = state
				switch {
				case !found || state == "pending" || state == "":
					pending = true
				case state == "connected":
					// The only status that means the tools are there.
				case state == "disabled" || state == "failed" || state == "needs_auth":
					// An answer, not a race: waiting the full timeout on a state
					// that will not change only delays the retry the driver owes.
					// `disabled` is included because a clankerbar entry that is
					// switched off leaves the session with no backlog tools at
					// all — the same outcome as a failed connection.
					return fmt.Errorf("%w: MCP server %q reports %q%s", ErrMCPNotConnected, name, state, serverDiagnostic(s.stderr))
				default:
					// An unrecognised status is not evidence of connection.
					pending = true
				}
			}
			if !pending {
				return nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("%w: MCP server(s) %s not connected within %s (status could not be read: %v)%s",
					ErrMCPNotConnected, strings.Join(names, ", "), opencodeMCPStartupTimeout, lastErr, serverDiagnostic(s.stderr))
			}
			return fmt.Errorf("%w: MCP server(s) %s not connected within %s (%s)%s",
				ErrMCPNotConnected, strings.Join(names, ", "), opencodeMCPStartupTimeout, statusSummary(names, last), serverDiagnostic(s.stderr))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opencodeMCPPollInterval):
		}
	}
}

// mcpStates reads the private server's MCP status for one location.
func (s *opencodeServer) mcpStates(ctx context.Context, dir string) (map[string]string, error) {
	endpoint := strings.TrimRight(s.url, "/") + "/api/mcp"
	if dir != "" {
		q := url.Values{}
		q.Set("location[directory]", dir)
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", s.password)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /api/mcp: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var doc struct {
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
		Data []struct {
			Name   string `json:"name"`
			Status struct {
				Status string `json:"status"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding /api/mcp: %w", err)
	}
	// The server keys its MCP state by location. An answer for ANY OTHER
	// location is not about the session that is about to run, so treating it as
	// this one's status is how a `/var` vs `/private/var` query reported
	// connected while the real session still had no tools (see opencodeWorkDir).
	//
	// An answer that does not identify its location at all is WORSE than a
	// different one: it cannot be told apart from a status for some other
	// session, and accepting it releases the first turn on no evidence. So both
	// "empty" and "different" are refused; only exact identity is accepted
	// (CLA-703 review, finding 2.1).
	if dir != "" {
		if doc.Location.Directory == "" {
			return nil, fmt.Errorf("the server's answer did not identify its location; wanted %q", dir)
		}
		if opencodeWorkDir(doc.Location.Directory) != dir {
			return nil, fmt.Errorf("the server answered for location %q, not the requested %q", doc.Location.Directory, dir)
		}
	}
	states := make(map[string]string, len(doc.Data))
	for _, entry := range doc.Data {
		states[entry.Name] = entry.Status.Status
	}
	return states, nil
}

// The file names opencode discovers by itself, in its own merge order (read off
// `opencode debug config` against 2.0.32; config's ambient-config audit walks the
// same list). `config.json` is a GLOBAL name only: a project-level config.json is
// an ordinary file thousands of repos carry for something else.
var (
	opencodeGlobalConfigNames  = []string{"config.json", "opencode.json", "opencode.jsonc"}
	opencodeProjectConfigNames = []string{"opencode.json", "opencode.jsonc", filepath.Join(".opencode", "opencode.json"), filepath.Join(".opencode", "opencode.jsonc")}
)

// opencodeEffectiveMCPServers names the clankerbar server(s) a session must have
// connected before its first turn, resolved across the config layers opencode
// actually loads.
//
// When the caller names an MCP config, that file is also pinned into
// OPENCODE_CONFIG_CONTENT — the layer opencode merges LAST — so the servers it
// declares are the effective ones. Only that file is read in that case: an
// ambient clankerbar entry cannot displace a server of the same name, and
// waiting for a differently-named ambient entry would stall the session on a
// server the backlog does not depend on.
//
// When the caller names NO file, however, opencode still loads its own layers,
// and they are then the only ones there are: the global config dir
// (OPENCODE_CONFIG_DIR when the caller pins one, else $XDG_CONFIG_HOME/opencode
// or ~/.config/opencode) and the project-level `opencode.json` /
// `.opencode/opencode.json` discovered from the INSTANCE DIRECTORY, then the
// content layer. A clankerbar server declared there is the session's backlog
// server just the same, so it is waited for here rather than discovered missing
// after the first model turn (CLA-703 review, findings 1.1 and 3.1).
//
// FAIL CLOSED: a layer that EXISTS but cannot be read or parsed leaves the
// effective config unknown, and "I could not tell whether a clankerbar server is
// declared" is not evidence that none is. A file that does not exist is simply
// not a layer. An empty result is a different, established answer: every layer
// was read and none declares a clankerbar server, so there is no name to wait
// for (the run still gets no backlog tools, which the driver's own no_mcp
// backstop classifies).
func opencodeEffectiveMCPServers(in Invocation, env []string) ([]string, error) {
	names := map[string]bool{}
	if in.MCPConfigPath != "" {
		// Strict JSON, the same rule config applies to every file the driver
		// names; env() has already failed the spawn if this file is unreadable,
		// so an error here is defensive, not a silent skip.
		if err := readOpencodeMCPDocument(in.MCPConfigPath, true, names); err != nil {
			return nil, fmt.Errorf("%w: cannot establish opencode's effective config: mcp config %s: %v",
				ErrMCPNotConnected, in.MCPConfigPath, err)
		}
		return sortedMCPNames(names), nil
	}

	read := func(path, source string) error {
		if err := readOpencodeMCPDocument(path, false, names); err != nil {
			return fmt.Errorf("%w: cannot establish opencode's effective config: %s: %v", ErrMCPNotConnected, source, err)
		}
		return nil
	}

	// 1. The global config dir. OPENCODE_CONFIG_DIR REPLACES the default one on
	// 2.x, so exactly one of the two is read.
	globalDir, err := opencodeGlobalConfigDir(env)
	if err != nil {
		return nil, err
	}
	for _, name := range opencodeGlobalConfigNames {
		path := filepath.Join(globalDir, name)
		if err := read(path, "global config "+path); err != nil {
			return nil, err
		}
	}
	// 2. OPENCODE_CONFIG. Normally empty when no file is named, but an explicit
	// caller value in in.Env is a layer opencode loads all the same.
	if path := envValue(env, "OPENCODE_CONFIG"); path != "" {
		if err := read(path, "OPENCODE_CONFIG "+path); err != nil {
			return nil, err
		}
	}
	// 3. Project-level files, discovered from the INSTANCE DIRECTORY ($PWD), not
	// from a parent: opencode reads the session's own tree.
	base := in.WorkDir
	if base == "" {
		base, _ = os.Getwd()
	}
	for _, name := range opencodeProjectConfigNames {
		path := filepath.Join(base, name)
		if err := read(path, "project config "+path); err != nil {
			return nil, err
		}
	}
	// 4. The content layer, merged last. With no named file the adapter's own
	// copy carries no `mcp`, but an explicit caller value in in.Env can.
	if content := envValue(env, "OPENCODE_CONFIG_CONTENT"); content != "" {
		if err := collectOpencodeMCPNames([]byte(content), names); err != nil {
			return nil, fmt.Errorf("%w: cannot establish opencode's effective config: OPENCODE_CONFIG_CONTENT: %v", ErrMCPNotConnected, err)
		}
	}
	return sortedMCPNames(names), nil
}

// readOpencodeMCPDocument adds the clankerbar servers of one config file. A file
// that does not exist contributes nothing; any other read or parse failure is an
// error, because the caller fails closed on it.
func readOpencodeMCPDocument(path string, strict bool, out map[string]bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !strict {
		data = jsonc.Strip(data)
	}
	return collectOpencodeMCPNames(data, out)
}

// collectOpencodeMCPNames decodes one JSON document and collects its clankerbar
// servers. Nothing about `mcp` yields nothing; a document that is not JSON is an
// error.
func collectOpencodeMCPNames(data []byte, out map[string]bool) error {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	collectOpencodeMCPServers(doc, out)
	return nil
}

// opencodeMCPServers names the clankerbar server(s) a NAMED config file
// declares. It is opencodeEffectiveMCPServers' named-file half, kept for the
// tests and for callers that already have a file in hand; the session path uses
// the resolver so an unnamed file cannot bypass the preflight.
func opencodeMCPServers(path string) []string {
	if path == "" {
		return nil
	}
	names := map[string]bool{}
	if err := readOpencodeMCPDocument(path, true, names); err != nil {
		return nil
	}
	return sortedMCPNames(names)
}

// collectOpencodeMCPServers adds every server in one document's `mcp` block that
// IS the clankerbar server: the documented name, or any entry handed
// CLANKERBAR_API_KEY (the config's own rule for which entry is the backlog's).
//
// The 2.x container (`mcp.servers`, an object of entries) and a 1.x server
// literally NAMED "servers" are told apart by the VALUE, the same rule config's
// mcpFile.UnmarshalJSON applies: the container's value carries none of the entry
// fields, while a 1.x entry carries at least one. Excluding the key outright
// skipped that supported legacy shape entirely (CLA-703 review, finding 1.2).
func collectOpencodeMCPServers(doc map[string]any, out map[string]bool) {
	mcp, ok := doc["mcp"].(map[string]any)
	if !ok {
		return
	}
	if raw, ok := mcp["servers"]; ok {
		if entries, ok := raw.(map[string]any); ok {
			if opencodeMCPEntryShaped(entries) {
				addOpencodeMCPServer("servers", entries, out)
			} else {
				for name, rawEntry := range entries {
					if entry, ok := rawEntry.(map[string]any); ok {
						addOpencodeMCPServer(name, entry, out)
					}
				}
			}
		}
	}
	// The 1.x dialect puts entries directly under `mcp`; 2.x accepts both shapes
	// in one file. `servers` was handled above, as either the container or a
	// server of that literal name.
	for name, raw := range mcp {
		if name == "servers" {
			continue
		}
		if entry, ok := raw.(map[string]any); ok {
			addOpencodeMCPServer(name, entry, out)
		}
	}
}

// addOpencodeMCPServer adds a server that IS the backlog's: the documented name,
// or any entry handed CLANKERBAR_API_KEY.
//
// A DISABLED entry is deliberately added too. Excluding it at config-read time
// would let a config-disabled clankerbar server through the preflight and only
// fail at the driver's backstop — after a full paid session with no backlog
// tools — while a runtime `disabled` status fails the preflight immediately.
// Adding it lets the status wait observe `disabled` and fail fast with
// ErrMCPNotConnected (CLA-703 review, finding 4.3).
func addOpencodeMCPServer(name string, entry map[string]any, out map[string]bool) {
	if name == "clankerbar" || opencodeMCPEntryUsesCredential(entry) {
		out[name] = true
	}
}

// opencodeMCPEntryShaped reports whether a value carries any field an MCP entry
// has, with a VALUE, exactly as config's entryIsZero reads the same document: an
// all-zero "servers" value is the 2.x container, one with any non-zero entry
// field is a 1.x server named "servers". The field set and the "non-zero" test
// mirror that decoder deliberately — an empty URL, a null map and an absent key
// are all zero there, so they are absent here.
func opencodeMCPEntryShaped(entry map[string]any) bool {
	if url, ok := entry["url"].(string); ok && url != "" {
		return true
	}
	for _, key := range []string{"headers", "env", "environment", "command", "args"} {
		if v, ok := entry[key]; ok && v != nil {
			return true
		}
	}
	for _, key := range []string{"enabled", "disabled"} {
		if _, ok := entry[key].(bool); ok {
			return true
		}
	}
	return false
}

func sortedMCPNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// opencodeGlobalConfigDir resolves the ONE global config directory opencode
// reads: OPENCODE_CONFIG_DIR when the session pins one (it REPLACES the default
// on 2.x), else $XDG_CONFIG_HOME/opencode, else ~/.config/opencode. Unresolvable
// is fail-closed: the caller cannot know which layers apply.
func opencodeGlobalConfigDir(env []string) (string, error) {
	if dir := envValue(env, "OPENCODE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	base := envValue(env, "XDG_CONFIG_HOME")
	if base == "" {
		home := envValue(env, "HOME")
		if home == "" {
			return "", fmt.Errorf("%w: cannot establish opencode's effective config: OPENCODE_CONFIG_DIR, XDG_CONFIG_HOME and HOME are all unset",
				ErrMCPNotConnected)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "opencode"), nil
}

// envValue reads a variable from a prepared environment slice the way exec and
// opencode do: the LAST assignment wins, because in.Env is appended after the
// adapter's own values.
func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok {
			return v
		}
	}
	return ""
}

func opencodeMCPEntryUsesCredential(entry map[string]any) bool {
	b, err := json.Marshal(entry)
	if err != nil {
		return false
	}
	// The literal is the config package's credentialEnvVar; the harness cannot
	// import config (config imports harness), and this is the same text the
	// operator writes in the file.
	return strings.Contains(string(b), "CLANKERBAR_API_KEY")
}

// mcpPreflightFailure classifies a failure to get the private server up. The
// per-session wall clock and a run-wide cancellation are NOT infrastructure
// errors: the first is the cap doing its job (the driver reads the marker and
// ends the phase), and the second is the caller stopping the run. Only a real
// preflight failure reaches the driver as ErrMCPNotConnected.
func mcpPreflightFailure(sctx, ctx context.Context, err error) (Result, error) {
	if sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		var res Result
		res.ExitCode = -1
		res.markWallClockCapped()
		return res, nil
	}
	if ctx.Err() != nil {
		// A shutdown inside the preflight window is NOT a clean exit. Returning
		// a zero Result would reach the driver as ExitCode 0 with no error — a
		// review-only phase would book it no_mcp and a panel would record the
		// member failed, while other phases walk the held-but-empty ladder and
		// spawn one doomed backstop. The driver's `ierr != nil && ctx.Err() != nil`
		// path is the shutdown classification, so hand it the cancellation
		// (CLA-703 review, finding 4.1).
		return Result{}, ctx.Err()
	}
	return Result{}, err
}

// withOpencodeServer turns a `run --standalone` argv into one attached to the
// private server. --standalone is REMOVED: it asks the CLI to spawn a second,
// private server, which is exactly the server whose MCP startup we just waited
// out and would reintroduce the race against a fresh one.
func withOpencodeServer(args []string, serverURL string) []string {
	out := make([]string, 0, len(args)+2)
	for _, a := range args {
		if a == "--standalone" {
			continue
		}
		out = append(out, a)
	}
	if len(out) > 0 && out[0] == "run" {
		withFlag := []string{"run", "--server", serverURL}
		return append(withFlag, out[1:]...)
	}
	return append([]string{"--server", serverURL}, out...)
}

func statusSummary(names []string, states map[string]string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		state := states[name]
		if state == "" {
			state = "not listed"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", name, state))
	}
	return strings.Join(parts, ", ")
}

func serverDiagnostic(stderr *tail) string {
	if stderr == nil {
		return ""
	}
	text := strings.TrimSpace(stderr.String())
	if text == "" {
		return ""
	}
	return ": " + text
}
