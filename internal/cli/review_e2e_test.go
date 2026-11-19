package cli

// CLA-870: the standalone review, end to end through the real CLI, the real
// opencode adapter and a real git repository. The stub below emits the adapter's
// own event stream, so the test proves the whole path — flags, config load,
// temporary worktree, stream parse, done-report check and JSON write — not a
// seam assembled by the test.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

func reviewE2EGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func reviewE2ECommit(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "change.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewE2EGit(t, dir, "add", "-A")
	reviewE2EGit(t, dir, "-c", "user.email=review@example.com", "-c", "user.name=review e2e", "commit", "-q", "-m", content)
	return reviewE2EGit(t, dir, "rev-parse", "HEAD")
}

func reviewE2EStub(t *testing.T, report string) {
	t.Helper()
	dir := t.TempDir()
	// The adapter may export the session after the run; refusing it keeps the
	// stream's figures and is what the harness package's own stubs do.
	//
	// printf, never echo: the report is Go-quoted, and dash's echo expands the
	// \n/\", splitting one JSON event across lines on a dash-based runner.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = session ] && [ \"$2\" = export ]; then exit 1; fi\n" +
		fmt.Sprintf(`case "$*" in
  *"You are reviewing"*)
    printf '%%s\n' '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":%q}}'
    printf '%%s\n' '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"stop","tokens":{"total":30,"input":10,"output":5,"reasoning":0,"cache":{"write":0,"read":15}},"cost":0.03}}'
    ;;
  *)
    printf '%%s\n' '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":"unexpected prompt"}}'
    ;;
esac
`, report)
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestReviewRunEndToEnd(t *testing.T) {
	repo := t.TempDir()
	reviewE2EGit(t, repo, "init", "-q")
	reviewE2EGit(t, repo, "config", "commit.gpgsign", "false")
	base := reviewE2ECommit(t, repo, "base\n")
	head := reviewE2ECommit(t, repo, "head\n")

	// An empty HOME is what makes config.Load hermetic: no operator config, no
	// env commands, and none of the settings a developer's ~/.config carries.
	t.Setenv("HOME", t.TempDir())

	report := fmt.Sprintf(`{"verdict":"findings","reviewedTip":%q,"findings":[{"severity":"must","summary":"The guard is missing","evidenceKind":"gate","evidence":"go test ./... fails"}],"omitted":1}`, head)
	reviewE2EStub(t, "Reviewed.\n\n"+config.DoneMarker+"\n"+report)

	out := filepath.Join(t.TempDir(), "review.json")
	var stdout, stderr bytes.Buffer
	if err := reviewRun(context.Background(), []string{
		"run", "--repo", repo, "--base", base, "--head", head, "--harness", "opencode", "--json", out,
	}, &stdout, &stderr); err != nil {
		t.Fatalf("review run: %v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: --json sent the report to a file", stdout.String())
	}
	if !strings.Contains(stderr.String(), out) {
		t.Errorf("stderr = %q, want the report path", stderr.String())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got loop.ReviewRunReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, data)
	}
	if got.Base != base || got.Head != head {
		t.Errorf("commits = %s..%s, want the resolved %s..%s", got.Base, got.Head, base, head)
	}
	if got.Method != config.ReviewMethodSinglePass || got.Turns != 1 {
		t.Errorf("method/turns = %q/%d, want single-pass/1", got.Method, got.Turns)
	}
	if len(got.Findings) != 1 || got.Findings[0].Severity != "must" || got.Omitted != 1 {
		t.Errorf("findings = %+v, omitted = %d, want the one MUST and 1 omitted", got.Findings, got.Omitted)
	}
	if got.Usage.Input != 10 || got.Usage.Output != 5 || got.Usage.CacheRead != 15 || got.Usage.CostUSD != 0.03 {
		t.Errorf("usage = %+v, want the stream's figures", got.Usage)
	}
	if got.NotesTurns == nil || len(got.NotesTurns) != 0 {
		t.Errorf("notes_turns = %#v, want an empty list for single-pass", got.NotesTurns)
	}
	if worktrees := reviewE2EGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Errorf("the repository keeps the review's worktree metadata:\n%s", worktrees)
	}
}

// reviewE2ENoBacklogStub is an `opencode` that fails the run outright if the
// adapter started a private server, attached to one, or handed over a content
// layer without the backlog off switch. Everything else about the run is the
// usual stub: one text part and a step_finish.
func reviewE2ENoBacklogStub(t *testing.T, report string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = session ] && [ \"$2\" = export ]; then exit 1; fi\n" +
		"if [ \"$1\" = serve ]; then exit 9; fi\n" + // no private server for a no-backlog review
		"case \"$*\" in *--server*) exit 8 ;; esac\n" + // and no attach
		"case \"$*\" in *--standalone*) ;; *) exit 6 ;; esac\n" +
		"case \"$OPENCODE_CONFIG_CONTENT\" in *'\"url\":\"https://clankerbar.com/mcp/makespdf\"'*) ;; *) exit 5 ;; esac\n" + // the checkout's own entry
		"case \"$OPENCODE_CONFIG_CONTENT\" in *'\"disabled\":true'*) ;; *) exit 7 ;; esac\n" + // switched off in the merged-last layer
		fmt.Sprintf(`case "$*" in
  *"You are reviewing"*)
    printf '%%s\n' '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":%q}}'
    printf '%%s\n' '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"stop","tokens":{"total":30,"input":10,"output":5,"reasoning":0,"cache":{"write":0,"read":15}},"cost":0.03}}'
    ;;
  *)
    printf '%%s\n' '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":"unexpected prompt"}}'
    ;;
esac
`, report)
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// CLA-927 end to end: a reviewed checkout that CHECKS IN a clankerbar MCP server
// still completes, the adapter neither starts nor attaches a private server, and
// the content layer carries the off switch. Both halves of the incident are
// covered because the reviewed repo's entry carries {env:CLANKERBAR_API_KEY}: with
// the variable set a pre-fix run connected and was offered the operator's backlog
// tools; without it the preflight failed needs_auth. Neither is possible when the
// server is switched off.
func TestReviewRunEndToEnd_CheckedInClankerbarServer(t *testing.T) {
	for _, key := range []string{"", "sk-live"} {
		t.Run("CLANKERBAR_API_KEY="+key, func(t *testing.T) {
			t.Setenv("CLANKERBAR_API_KEY", key)
			// Hermetic config: no operator config, and no real global opencode dir.
			// (This suite runs inside an opencode session whose environment names
			// its own config dir and content layer; the content one is replaced by
			// every spawned session, the dir is not.)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())

			repo := t.TempDir()
			reviewE2EGit(t, repo, "init", "-q")
			reviewE2EGit(t, repo, "config", "commit.gpgsign", "false")
			base := reviewE2ECommit(t, repo, "base\n")
			if err := os.WriteFile(filepath.Join(repo, "opencode.json"), []byte(`{
  "mcp": {
    "servers": {
      "clankerbar": {
        "type": "remote",
        "url": "https://clankerbar.com/mcp/makespdf",
        "headers": {"Authorization": "Bearer {env:CLANKERBAR_API_KEY}"}
      }
    }
  }
}`), 0o644); err != nil {
				t.Fatal(err)
			}
			head := reviewE2ECommit(t, repo, "head\n")

			report := fmt.Sprintf(`{"verdict":"findings","reviewedTip":%q,"findings":[{"severity":"must","summary":"The guard is missing","evidenceKind":"gate","evidence":"go test ./... fails"}],"omitted":0}`, head)
			reviewE2ENoBacklogStub(t, "Reviewed.\n\n"+config.DoneMarker+"\n"+report)

			out := filepath.Join(t.TempDir(), "review.json")
			var stdout, stderr bytes.Buffer
			if err := reviewRun(context.Background(), []string{
				"run", "--repo", repo, "--base", base, "--head", head, "--json", out,
			}, &stdout, &stderr); err != nil {
				t.Fatalf("review run: %v\nstderr:\n%s", err, stderr.String())
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var got loop.ReviewRunReport
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("the report is not JSON: %v\n%s", err, data)
			}
			if got.Base != base || got.Head != head || got.Turns != 1 {
				t.Errorf("commits/turns = %s..%s/%d, want %s..%s/1", got.Base, got.Head, got.Turns, base, head)
			}
		})
	}
}
