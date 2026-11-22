package cli

// CLA-975: `clankerbar review map`'s flag vocabulary, its two output paths, and
// the two refusals the command owns before loop.ReviewMap touches git.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

func parseReviewMap(t *testing.T, args ...string) (reviewMapFlags, error) {
	t.Helper()
	var f reviewMapFlags
	fs := newReviewMapFlagSet(&f)
	fs.SetOutput(&bytes.Buffer{})
	err := parseFlags(fs, args)
	return f, err
}

// reviewMapCLIRepo is a real checkout with two commits, so the command's git
// plumbing runs for real.
func reviewMapCLIRepo(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo = t.TempDir()
	reviewE2EGit(t, repo, "init", "-q")
	reviewE2EGit(t, repo, "config", "commit.gpgsign", "false")
	base = reviewE2ECommit(t, repo, "base\n")
	head = reviewE2ECommit(t, repo, "head\n")
	return repo, base, head
}

func TestReviewMapFlagDefaults(t *testing.T) {
	f, err := parseReviewMap(t, "--base", "abc123")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts, jsonPath, err := reviewMapOptions(f)
	if err != nil {
		t.Fatalf("reviewMapOptions: %v", err)
	}
	if opts.Repo != "." || opts.Head != "HEAD" {
		t.Errorf("repo/head = %q/%q, want ./HEAD", opts.Repo, opts.Head)
	}
	if opts.ChunkSize != 0 || opts.FlowMax != 0 {
		t.Errorf("chunk/flow = %d/%d, want 0/0 (resolved defaults)", opts.ChunkSize, opts.FlowMax)
	}
	if opts.Base != "abc123" {
		t.Errorf("base = %q, want abc123", opts.Base)
	}
	if jsonPath != "" {
		t.Errorf("json = %q, want stdout", jsonPath)
	}
}

func TestReviewMapFlagContextAndKnobs(t *testing.T) {
	desc := filepath.Join(t.TempDir(), "description.md")
	if err := os.WriteFile(desc, []byte("It drops the last token.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := parseReviewMap(t, "--base", "b", "--head", "h", "--repo", "/tmp/checkout",
		"--chunk-size", "7", "--flow-max", "9", "--title", "Fix it",
		"--description-file", desc, "--json", "out.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts, jsonPath, err := reviewMapOptions(f)
	if err != nil {
		t.Fatalf("reviewMapOptions: %v", err)
	}
	if opts.ChunkSize != 7 || opts.FlowMax != 9 || opts.Repo != "/tmp/checkout" || opts.Head != "h" {
		t.Errorf("options = %+v, want the parsed repo, head and maps knobs", opts)
	}
	if opts.Title != "Fix it" || !strings.Contains(opts.Description, "drops the last token") {
		t.Errorf("context = %q / %q, want the title and the description file's text", opts.Title, opts.Description)
	}
	if jsonPath != "out.json" {
		t.Errorf("json path = %q, want out.json", jsonPath)
	}
}

func TestReviewMapFlagRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing base", []string{}, "--base is required"},
		{"empty base", []string{"--base", "  "}, "--base is required"},
		{"negative chunk", []string{"--base", "b", "--chunk-size", "-1"}, "must not be negative"},
		{"negative flow", []string{"--base", "b", "--flow-max", "-1"}, "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseReviewMap(t, tc.args...)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if _, _, err := reviewMapOptions(f); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to carry %q", err, tc.want)
			}
		})
	}
}

// The map command has no session knobs at all: a script that passes one gets a
// parse error rather than a plan whose method someone thought they changed.
func TestReviewMapRefusesSessionFlags(t *testing.T) {
	for _, flag := range []string{"--method", "--harness", "--model", "--notes-model", "--findings-model", "--embed-hunks"} {
		args := []string{"--base", "b", flag}
		if flag != "--embed-hunks" {
			args = append(args, "x")
		}
		if _, err := parseReviewMap(t, args...); err == nil {
			t.Errorf("parse accepted %s, which review map does not take", flag)
		}
	}
}

func TestReviewMapMissingDescriptionFile(t *testing.T) {
	f, err := parseReviewMap(t, "--base", "b", "--description-file", filepath.Join(t.TempDir(), "nope.md"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, err := reviewMapOptions(f); err == nil || !strings.Contains(err.Error(), "nope.md") {
		t.Fatalf("error = %v, want the missing description file named", err)
	}
}

func TestReviewMapHelpSucceeds(t *testing.T) {
	_, err := parseReviewMap(t, "--help")
	if !errors.Is(err, pflag.ErrHelp) {
		t.Fatalf("error = %v, want pflag.ErrHelp (help exits 0)", err)
	}
}

func TestReviewMapWritesFileOrStdout(t *testing.T) {
	repo, base, head := reviewMapCLIRepo(t)
	path := filepath.Join(t.TempDir(), "plan.json")

	var stdout, stderr bytes.Buffer
	if err := reviewMap([]string{"--repo", repo, "--base", base, "--head", head, "--json", path}, &stdout, &stderr); err != nil {
		t.Fatalf("reviewMap --json: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing when --json names a file", stdout.String())
	}
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("stderr = %q, want the plan path", stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fromFile loop.ReviewMapReport
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("the written plan does not parse: %v\n%s", err, data)
	}
	if fromFile.Head != head || len(fromFile.Turns) < 2 {
		t.Errorf("written plan = %+v, want the resolved head and the turns", fromFile)
	}
	if last := fromFile.Turns[len(fromFile.Turns)-1]; last.Kind != "findings" {
		t.Errorf("last written turn = %+v, want findings", last)
	}

	stdout.Reset()
	stderr.Reset()
	if err := reviewMap([]string{"--repo", repo, "--base", base, "--head", head}, &stdout, &stderr); err != nil {
		t.Fatalf("reviewMap to stdout: %v", err)
	}
	var fromStdout loop.ReviewMapReport
	if err := json.Unmarshal(stdout.Bytes(), &fromStdout); err != nil {
		t.Fatalf("stdout is not the JSON document: %v\n%s", err, stdout.String())
	}
	if fromStdout.Base != base || len(fromStdout.Turns) != len(fromFile.Turns) {
		t.Errorf("stdout plan = %+v, want the same plan the file got", fromStdout)
	}
	if !strings.Contains(stderr.String(), "review map:") {
		t.Errorf("stderr = %q, want the progress line", stderr.String())
	}
}

// TestReviewMapLoadsNoConfig: the command is documented to work on a machine
// with no clankerbar config, so a config that would make config.Load fail must
// not reach it.
func TestReviewMapLoadsNoConfig(t *testing.T) {
	repo, base, head := reviewMapCLIRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "clankerbar")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(""); err == nil {
		t.Fatal("the fixture config loads: this test would not prove the command skips config.Load")
	}

	var stdout, stderr bytes.Buffer
	if err := reviewMap([]string{"--repo", repo, "--base", base, "--head", head}, &stdout, &stderr); err != nil {
		t.Fatalf("reviewMap read the operator's config: %v", err)
	}
}

func TestReviewUnknownSubcommandNamesBothCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := reviewRun(context.Background(), []string{"mapp"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("reviewRun accepted an unknown subcommand")
	}
	for _, want := range []string{`"mapp"`, "clankerbar review run", "clankerbar review map"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
	if !strings.Contains(stderr.String(), "review map") {
		t.Errorf("the usage printed on a bad subcommand does not mention review map:\n%s", stderr.String())
	}

	var emptyOut, emptyErr bytes.Buffer
	if err := reviewRun(context.Background(), nil, &emptyOut, &emptyErr); err == nil {
		t.Fatal("reviewRun accepted no subcommand")
	} else if !strings.Contains(err.Error(), "review map") {
		t.Errorf("the no-subcommand error = %v, want it to name both commands", err)
	}
}

func TestReviewHelpDescribesMap(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := reviewRun(context.Background(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("review help: %v", err)
	}
	for _, want := range []string{"clankerbar review map", "starts no session", "must be at --head"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("reviewUsage does not carry %q:\n%s", want, stdout.String())
		}
	}
}
