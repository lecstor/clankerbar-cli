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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// opencodeMCPStartupTimeout bounds the whole preflight — the server announcing
// itself AND the clankerbar server reporting connected — and is a var so tests
// can shrink it. Thirty seconds is six times opencode's own default MCP startup
// timeout (5s), which leaves room for a remote server's handshake and one
// opencode-side retry without making a broken MCP config look like a hang.
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
				case state == "connected" || state == "disabled":
					// An answer. disabled is the operator's choice, not a race.
				case state == "failed" || state == "needs_auth":
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
	states := make(map[string]string, len(doc.Data))
	for _, entry := range doc.Data {
		states[entry.Name] = entry.Status.Status
	}
	return states, nil
}

// opencodeMCPServers names the MCP servers a session must have before its first
// turn: the clankerbar one(s) declared by the MCP config file the adapter pins.
//
// The config file is read as strict JSON, the same rule config applies to every
// file the driver names. A file that cannot be read or parsed names nothing —
// env() has already refused the spawn by then, so this is a defensive empty,
// not a silent skip.
//
// An EMPTY path means the caller declared no MCP config at all (a single-phase
// run that wants no backlog, or a harness falling back to its own config dir).
// There is nothing to wait for by NAME that would not be a guess: waiting for
// "clankerbar" there would stall every such session on a server it may not have
// been asked for. The phased runs this fix exists for always name a file, and
// config.Validate refuses one that does not declare the clankerbar server.
func opencodeMCPServers(path string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	names := map[string]bool{}
	if mcp, ok := doc["mcp"].(map[string]any); ok {
		if servers, ok := mcp["servers"].(map[string]any); ok {
			collectOpencodeMCPServers(servers, names)
		}
		// The 1.x dialect (`mcp.<name>` with a type) is still accepted by 2.x;
		// the adapter rewrites it into `servers`, but the file is read here in
		// whatever dialect the operator wrote.
		collectOpencodeMCPServers(mcp, names)
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// collectOpencodeMCPServers adds every enabled server that IS the clankerbar
// server: the documented name, or any entry handed CLANKERBAR_API_KEY (the
// config's own rule for which entry is the backlog's).
func collectOpencodeMCPServers(servers map[string]any, out map[string]bool) {
	for name, raw := range servers {
		if name == "servers" {
			continue
		}
		entry, ok := raw.(map[string]any)
		if !ok || !opencodeMCPEntryEnabled(entry) {
			continue
		}
		if name == "clankerbar" || opencodeMCPEntryUsesCredential(entry) {
			out[name] = true
		}
	}
}

func opencodeMCPEntryEnabled(entry map[string]any) bool {
	// 1.x `enabled: false` and 2.x `disabled: true` are the same statement.
	if enabled, ok := entry["enabled"].(bool); ok && !enabled {
		return false
	}
	if disabled, ok := entry["disabled"].(bool); ok && disabled {
		return false
	}
	return true
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
		return Result{}, nil
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
