// Command release-tool is the release pipeline's two decisions, in one binary
// the workflows call: what version a promotion implies, and whether the `ci`
// check on the commit that ships actually passed.
//
// It is DEVELOPMENT tooling and is deliberately not part of the shipped product:
// .goreleaser.yaml builds `./cmd/clankerbar` only, so nothing here reaches a
// published archive. It exists as Go rather than as shell in the workflow for
// one reason - the rules it encodes (an absent check is not a pass; a docs-only
// promotion publishes nothing; an unrecognised commit is a patch, not a no-op)
// are exactly the rules that must be pinned by tests, and YAML cannot be tested.
// The logic lives in internal/release; this file is only argument parsing, git
// and HTTP.
//
// Usage:
//
//	git log --no-merges --format=%B%x00 v0.3.4..HEAD |
//	  release-tool next-version --current v0.3.4
//
//	release-tool await-check --repo lecstor/clankerbar-cli --sha "$SHA" \
//	  --required ci --timeout 20m --interval 15s
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release-tool <next-version|await-check> [flags]")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "next-version":
		err = nextVersion(os.Args[2:])
	case "await-check":
		err = awaitCheck(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q (want next-version or await-check)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "release-tool: %v\n", err)
		os.Exit(1)
	}
}

// flags is a tiny --key value parser. pflag is the repo's one dependency and is
// used by the product CLI; this tool takes no dependency at all so that a
// release decision can never be blocked on resolving one.
func flags(args []string) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return nil, fmt.Errorf("unexpected argument %q", a)
		}
		k := strings.TrimPrefix(a, "--")
		if eq := strings.Index(k, "="); eq >= 0 {
			out[k[:eq]] = k[eq+1:]
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag --%s has no value", k)
		}
		out[k] = args[i+1]
		i++
	}
	return out, nil
}

// nextVersion derives the tag a promotion should publish and writes the result
// as GitHub Actions `key=value` output lines, appended to $GITHUB_OUTPUT when it
// is set and printed to stdout either way so a hand-run is readable.
//
// Commit messages arrive NUL-separated on stdin, which is what
// `git log --format=%B%x00` produces. NUL rather than newline because a commit
// BODY contains newlines - the breaking-change footer this reads is on its own
// line, so a line-based split would shred exactly the input that matters.
func nextVersion(args []string) error {
	f, err := flags(args)
	if err != nil {
		return err
	}
	current := f["current"]
	if current == "" {
		return fmt.Errorf("--current <tag> is required")
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading commit messages from stdin: %w", err)
	}

	var messages []string
	for _, m := range strings.Split(string(raw), "\x00") {
		if strings.TrimSpace(m) != "" {
			messages = append(messages, m)
		}
	}

	d, err := release.NextFromCommits(current, messages)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "%d releasable-range commit(s) since %s imply a %s bump\n",
		len(messages), d.Current, d.Bump)
	if !d.Release {
		// The docs-only case. Skipping quietly is the decided behaviour: an empty
		// version bump would publish a release nobody can tell apart from the last
		// one, and failing the workflow would make a docs promotion look broken.
		fmt.Fprintln(os.Stderr, "nothing releasable in this range - no tag, no release")
	}

	out := fmt.Sprintf("release=%t\nversion=%s\nbump=%s\nprevious=%s\n",
		d.Release, d.Next, d.Bump, d.Current)
	fmt.Print(out)

	if p := os.Getenv("GITHUB_OUTPUT"); p != "" {
		fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("opening GITHUB_OUTPUT: %w", err)
		}
		defer fh.Close()
		if _, err := fh.WriteString(out); err != nil {
			return fmt.Errorf("writing GITHUB_OUTPUT: %w", err)
		}
	}
	return nil
}

// awaitCheck blocks until the required check on a commit reaches a verdict, and
// exits NON-ZERO on anything but a pass - including a check that never appears.
// Exiting non-zero is the gate: the workflow step fails, and no tag is pushed.
func awaitCheck(args []string) error {
	f, err := flags(args)
	if err != nil {
		return err
	}
	repo, sha := f["repo"], f["sha"]
	if repo == "" || sha == "" {
		return fmt.Errorf("--repo <owner/name> and --sha <sha> are required")
	}
	required := f["required"]
	if required == "" {
		required = release.DefaultRequiredCheck
	}

	timeout := 20 * time.Minute
	if v := f["timeout"]; v != "" {
		if timeout, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
	}
	interval := 15 * time.Second
	if v := f["interval"]; v != "" {
		if interval, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("--interval: %w", err)
		}
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		// Not a warning. Without a token the API answers 404 for a private repo
		// and an unauthenticated rate limit for a public one, and BOTH look like
		// "no checks" - which is the one state that must never be mistaken for
		// anything benign. Refuse up front rather than gate on garbage.
		return fmt.Errorf("GITHUB_TOKEN is unset; refusing to gate a release on unauthenticated check reads")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s/check-runs?per_page=100", repo, sha)

	fetch := func() ([]release.CheckRun, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET check-runs: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}

		var payload struct {
			CheckRuns []release.CheckRun `json:"check_runs"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decoding check-runs: %w", err)
		}
		return payload.CheckRuns, nil
	}

	fmt.Fprintf(os.Stderr, "waiting up to %s for check %q on %s@%s\n", timeout, required, repo, sha)
	verdict, reason := release.Await(required, fetch, timeout, interval, release.RealClock{})
	fmt.Fprintf(os.Stderr, "verdict: %s - %s\n", verdict, reason)

	if verdict != release.VerdictPass {
		return fmt.Errorf("release gate refused: %s", reason)
	}
	return nil
}
