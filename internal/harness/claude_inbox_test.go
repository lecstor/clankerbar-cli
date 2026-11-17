package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeClaudeStub installs an executable `claude` on PATH. The script can address its own
// argv/stdin logs through $ARGV and $STDIN.
func writeClaudeStub(t *testing.T, script string) (stdinLog, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	stdinLog = filepath.Join(dir, "stdin.log")
	argvLog = filepath.Join(dir, "argv.log")
	body := fmt.Sprintf("ARGV=%q\nSTDIN=%q\n", argvLog, stdinLog) + script
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("writing claude stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stdinLog, argvLog
}

func readStubFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// The whole delivery seam: the prompt goes to stdin as a stream-json user turn (not argv), a
// console message is polled and written as the next user turn, its ACK is posted, and the
// session stays open across the first turn because that message is outstanding.
func TestClaudeDeliversAConsoleMessageToStdin(t *testing.T) {
	stdinLog, argvLog := writeClaudeStub(t, `
printf '%s\n' "$@" > "$ARGV"
IFS= read -r l1; printf '%s\n' "$l1" >> "$STDIN"
IFS= read -r l2; printf '%s\n' "$l2" >> "$STDIN"
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"turn one"}]}}'
echo '{"type":"result","subtype":"success","result":"first done","usage":{}}'
echo '{"type":"result","subtype":"success","result":"second done","usage":{}}'
`)

	var mu sync.Mutex
	var acks []int64
	pollAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			var body struct {
				Seq int64 `json:"seq"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// A slow plane: the session ends while this ACK is in flight, and Invoke must
			// not return before it lands. Keep it between Invoke's exit latency and requestTimeout.
			time.Sleep(time.Second)
			mu.Lock()
			acks = append(acks, body.Seq)
			mu.Unlock()
			fmt.Fprint(w, `{"acked":1}`)
			return
		}
		mu.Lock()
		pollAuth = r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Query().Get("after") == "0" {
			fmt.Fprint(w, `{"messages":[{"id":"m1","seq":1,"at":"2026-11-16T12:00:00Z","body":"also update the changelog"}],"next":1,"ended":false}`)
			return
		}
		fmt.Fprint(w, `{"messages":[],"next":1,"ended":false}`)
	}))
	defer srv.Close()

	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:  "Work the backlog.",
		Console: io.Discard,
		Inbox: &InboxConfig{
			Origin:       srv.URL,
			APIKey:       "clk_test",
			PollInterval: 10 * time.Millisecond,
		},
		// A resumed phase already knows its run; the fresh-claim path is the next test.
		ResumeClaim: Claim{TaskID: "t-1", RunID: "run.cred"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}

	argv := readStubFile(t, argvLog)
	if !strings.Contains(argv, "--input-format\nstream-json") {
		t.Errorf("argv = %q, want --input-format stream-json", argv)
	}
	if strings.Contains(argv, "Work the backlog.") {
		t.Errorf("argv carries the prompt: %q — it must travel on stdin", argv)
	}

	lines := strings.Split(strings.TrimSpace(readStubFile(t, stdinLog)), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdin lines = %d (%q), want the prompt then the console message", len(lines), lines)
	}
	for i, want := range []string{"Work the backlog.", "also update the changelog"} {
		var msg userMessage
		if err := json.Unmarshal([]byte(lines[i]), &msg); err != nil {
			t.Fatalf("stdin line %d is not JSON: %v (%q)", i+1, err, lines[i])
		}
		if msg.Type != "user" || msg.Message.Role != "user" {
			t.Errorf("line %d = %+v, want a user message", i+1, msg)
		}
		if len(msg.Message.Content) != 1 || msg.Message.Content[0].Text != want {
			t.Errorf("line %d content = %+v, want %q", i+1, msg.Message.Content, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if pollAuth != "Bearer clk_test" {
		t.Errorf("poll Authorization = %q", pollAuth)
	}
	if len(acks) == 0 || acks[len(acks)-1] != 1 {
		t.Errorf("acks = %v, want the delivered watermark 1", acks)
	}
}

// A fresh session learns its run id from its own claim_task result; delivery must start then,
// not only for a resumed phase.
func TestClaudeStartsDeliveryFromTheObservedClaim(t *testing.T) {
	writeClaudeStub(t, `
printf '%s\n' "$@" > "$ARGV"
IFS= read -r l1; printf '%s\n' "$l1" >> "$STDIN"
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"mcp__clankerbar__claim_task","input":{"taskId":"t-1"}}]}}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":[{"type":"text","text":"{\"task\":{\"id\":\"t-1\"},\"run\":{\"id\":\"run.claimed\"}}"}]}]}}'
IFS= read -r l2; printf '%s\n' "$l2" >> "$STDIN"
echo '{"type":"result","subtype":"success","result":"done","usage":{}}'
echo '{"type":"result","subtype":"success","result":"done","usage":{}}'
`)

	var mu sync.Mutex
	paths := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			fmt.Fprint(w, `{}`)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Query().Get("after") == "0" {
			fmt.Fprint(w, `{"messages":[{"id":"m1","seq":1,"at":"2026-11-16T12:00:00Z","body":"steer"}],"next":1,"ended":false}`)
			return
		}
		fmt.Fprint(w, `{"messages":[],"next":1,"ended":false}`)
	}))
	defer srv.Close()

	if _, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:  "work",
		Console: io.Discard,
		Inbox:   &InboxConfig{Origin: srv.URL, APIKey: "k", PollInterval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("the poller never ran: no request after the observed claim")
	}
	if paths[0] != "/api/runs/run.claimed/inbox" {
		t.Errorf("first poll path = %q, want the claimed run's inbox", paths[0])
	}
}

// A slow or failing poll is NOT the session's problem: Invoke returns when the session's own
// stream ends, even with the plane request still hanging.
func TestClaudeSessionDoesNotWaitForASlowPoll(t *testing.T) {
	writeClaudeStub(t, `
printf '%s\n' "$@" > "$ARGV"
IFS= read -r l1; printf '%s\n' "$l1" >> "$STDIN"
echo '{"type":"result","subtype":"success","result":"done","usage":{}}'
`)

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hangs until the test releases it
	}))
	defer srv.Close()
	defer close(block)

	start := time.Now()
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:      "work",
		Console:     io.Discard,
		ResumeClaim: Claim{TaskID: "t", RunID: "run"},
		Inbox:       &InboxConfig{Origin: srv.URL, APIKey: "k", PollInterval: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Invoke took %s with a hung poll — a slow plane must not stall the session", elapsed)
	}
}
