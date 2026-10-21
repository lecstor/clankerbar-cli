package main

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
	"path/filepath"
	"strings"
	"time"
)

// The fleet does not run `opencode run --standalone` for a working session: it
// starts a private `opencode serve` with the same pinned config, waits until
// the clankerbar MCP server reports `connected`, and only THEN attaches the run
// with `--server`. opencode 2.x connects MCP asynchronously, so a run that is
// submitted immediately can take its first turn with no clankerbar tools at
// all. That is not hypothetical: the rig's first live padding run did exactly
// it (the model answered "no clankerbar MCP tools are exposed"), which is why
// this file exists.
//
// The rig mirrors the adapter's preflight (internal/harness/opencode_mcp.go),
// including the location key: the status query must identify the SAME resolved
// directory the session will use, or a `/var` vs `/private/var` mismatch reports
// connected for a location no session will ever touch.
const (
	rigServerStartTimeout = 30 * time.Second
	rigMCPTimeout         = 30 * time.Second
	rigMCPPollInterval    = 100 * time.Millisecond
)

// rigServer is the private opencode server one rig run owns.
type rigServer struct {
	url      string
	password string
	cmd      *exec.Cmd
	exited   chan struct{}
	stderr   *os.File
}

// startRigServer launches `opencode serve` and waits for it to announce its
// address and password. The password is kept in memory only: the announcement
// line is the one line not written to the run's log.
func startRigServer(bin string, env []string, dir, out string) (*rigServer, error) {
	cmd := exec.Command(bin, "--print-logs", "serve", "--port", "0", "--hostname", "127.0.0.1")
	cmd.Dir = dir
	cmd.Env = env
	configureProcessGroup(cmd)

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = devNull
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		devNull.Close()
		return nil, err
	}
	stderrFile, err := os.Create(filepath.Join(out, "opencode-server-stderr.log"))
	if err != nil {
		devNull.Close()
		return nil, err
	}
	cmd.Stderr = stderrFile
	stdoutLog, err := os.Create(filepath.Join(out, "opencode-server-stdout.log"))
	if err != nil {
		devNull.Close()
		stderrFile.Close()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		devNull.Close()
		stderrFile.Close()
		stdoutLog.Close()
		return nil, fmt.Errorf("starting %s serve: %w", bin, err)
	}
	s := &rigServer{cmd: cmd, exited: make(chan struct{}), stderr: stderrFile}
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()
	// The scanner goroutine only WRITES announcements on the channel; the
	// fields are set by the goroutine that owns them (this one), so there is no
	// race and no lock.
	type announce struct{ kind, value string }
	announcements := make(chan announce, 4)
	go func() {
		defer devNull.Close()
		defer stdoutLog.Close()
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "server listening on "); ok {
				select {
				case announcements <- announce{"url", strings.TrimSpace(v)}:
				default:
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "server password "); ok {
				// Never write the password to the run's logs.
				select {
				case announcements <- announce{"password", strings.TrimSpace(v)}:
				default:
				}
				continue
			}
			fmt.Fprintln(stdoutLog, line)
		}
	}()

	deadline := time.Now().Add(rigServerStartTimeout)
	for s.url == "" || s.password == "" {
		if time.Now().After(deadline) {
			s.stop()
			return nil, fmt.Errorf("opencode serve did not announce its address and password within %s (see opencode-server-stderr.log)", rigServerStartTimeout)
		}
		select {
		case a := <-announcements:
			switch a.kind {
			case "url":
				s.url = a.value
			case "password":
				s.password = a.value
			}
		case <-s.exited:
			s.stop()
			return nil, fmt.Errorf("opencode serve exited before it announced its address and password (see opencode-server-stderr.log)")
		case <-time.After(20 * time.Millisecond):
		}
	}
	return s, nil
}

// stop kills the server's process group and waits briefly for its reaper. It is
// safe on nil and idempotent.
func (s *rigServer) stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	killProcessGroup(s.cmd)
	_ = s.cmd.Process.Kill()
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
	}
	if s.stderr != nil {
		_ = s.stderr.Close()
	}
}

// waitForMCP polls the server until the named MCP server is connected. A
// `failed`, `needs_auth` or `disabled` status is an answer, not a race, so it
// fails at once.
func (s *rigServer) waitForMCP(ctx context.Context, name, dir string, timeout time.Duration) error {
	resolved, err := resolveWorkdir(dir)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	last := "not listed"
	var lastErr error
	for {
		states, err := s.mcpStates(ctx, resolved)
		if err == nil {
			st := states[name]
			if st == "" {
				st = "not listed"
			}
			last = st
			switch st {
			case "connected":
				return nil
			case "disabled", "failed", "needs_auth":
				return fmt.Errorf("MCP server %q reports %q, so the session would run without the backlog tools", name, st)
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("MCP server %q not connected within %s (last status %q; status read: %v)", name, timeout, last, lastErr)
			}
			return fmt.Errorf("MCP server %q not connected within %s (last status %q)", name, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rigMCPPollInterval):
		}
	}
}

// mcpStates reads the private server's MCP status for one location.
func (s *rigServer) mcpStates(ctx context.Context, dir string) (map[string]string, error) {
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
	if dir != "" {
		if doc.Location.Directory == "" {
			return nil, fmt.Errorf("the server's answer did not identify its location; wanted %q", dir)
		}
		if got, err := resolveWorkdir(doc.Location.Directory); err != nil || got != dir {
			return nil, fmt.Errorf("the server answered for location %q, not %q", doc.Location.Directory, dir)
		}
	}
	states := make(map[string]string, len(doc.Data))
	for _, e := range doc.Data {
		states[e.Name] = e.Status.Status
	}
	return states, nil
}

// resolveWorkdir canonicalises a session directory the way the server keys its
// locations: absolute with symlinks resolved (macOS /var -> /private/var).
func resolveWorkdir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}
