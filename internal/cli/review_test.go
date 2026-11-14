package cli

// CLA-870: `clankerbar review run`'s flag vocabulary and pre-session
// validation. Nothing here starts a session: the parse step is separated from
// RunReview exactly so a typo is refused (and tested) without spending one.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

func parseReviewRun(t *testing.T, args ...string) (reviewRunFlags, error) {
	t.Helper()
	var f reviewRunFlags
	fs := newReviewRunFlagSet(&f)
	fs.SetOutput(io.Discard)
	err := parseFlags(fs, args)
	return f, err
}

func TestReviewRunFlagDefaults(t *testing.T) {
	f, err := parseReviewRun(t, "--base", "abc123")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts, jsonPath, err := reviewRunOptions(f)
	if err != nil {
		t.Fatalf("reviewRunOptions: %v", err)
	}
	if opts.Method != config.ReviewMethodSinglePass {
		t.Errorf("method = %q, want the single-pass default", opts.Method)
	}
	if opts.Harness != "opencode" {
		t.Errorf("harness = %q, want opencode (the maps method needs session continuation)", opts.Harness)
	}
	if opts.Head != "HEAD" {
		t.Errorf("head = %q, want HEAD", opts.Head)
	}
	if opts.Repo != "." {
		t.Errorf("repo = %q, want .", opts.Repo)
	}
	if jsonPath != "" {
		t.Errorf("json = %q, want stdout", jsonPath)
	}
	if opts.Base != "abc123" {
		t.Errorf("base = %q, want abc123", opts.Base)
	}
}

func TestReviewRunFlagMapsKnobs(t *testing.T) {
	desc := filepath.Join(t.TempDir(), "description.md")
	if err := os.WriteFile(desc, []byte("It drops the last token.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := parseReviewRun(t,
		"--base", "base", "--head", "head", "--method", "maps-session",
		"--repo", "/tmp/checkout", "--model", "phase", "--notes-model", "notes",
		"--findings-model", "findings", "--chunk-size", "7", "--flow-max", "9",
		"--title", "Fix it", "--description-file", desc, "--json", "out.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts, jsonPath, err := reviewRunOptions(f)
	if err != nil {
		t.Fatalf("reviewRunOptions: %v", err)
	}
	if opts.Method != config.ReviewMethodMapsSession || opts.ChunkSize != 7 || opts.FlowMax != 9 {
		t.Errorf("maps options = %+v, want the method and its knobs", opts)
	}
	if opts.NotesModel != "notes" || opts.FindingsModel != "findings" || opts.Model != "phase" {
		t.Errorf("models = %q/%q/%q, want phase/notes/findings", opts.Model, opts.NotesModel, opts.FindingsModel)
	}
	if opts.Title != "Fix it" || !strings.Contains(opts.Description, "drops the last token") {
		t.Errorf("context = %q / %q, want the title and the description file's text", opts.Title, opts.Description)
	}
	if jsonPath != "out.json" {
		t.Errorf("json path = %q, want out.json", jsonPath)
	}
}

func TestReviewRunFlagSingleAlias(t *testing.T) {
	f, err := parseReviewRun(t, "--base", "b", "--method", "single")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts, _, err := reviewRunOptions(f)
	if err != nil {
		t.Fatalf("reviewRunOptions: %v", err)
	}
	if opts.Method != config.ReviewMethodSinglePass {
		t.Errorf("method = %q, want the canonical single-pass", opts.Method)
	}
}

func TestReviewRunFlagRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing base", []string{}, "--base is required"},
		{"empty base", []string{"--base", "  "}, "--base is required"},
		{"unknown method", []string{"--base", "b", "--method", "panel"}, "unknown review method"},
		{"empty harness", []string{"--base", "b", "--harness", " "}, "--harness must name a harness"},
		{"notes model on single", []string{"--base", "b", "--notes-model", "x"}, "--notes-model"},
		{"findings model on single", []string{"--base", "b", "--findings-model", "x"}, "--findings-model"},
		{"chunk on single", []string{"--base", "b", "--chunk-size", "3"}, "--chunk-size"},
		{"flow on single", []string{"--base", "b", "--flow-max", "3"}, "--flow-max"},
		{"negative chunk", []string{"--base", "b", "--method", "maps-session", "--chunk-size", "-1"}, "must not be negative"},
		{"negative flow", []string{"--base", "b", "--method", "maps-session", "--flow-max", "-1"}, "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseReviewRun(t, tc.args...)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, _, err = reviewRunOptions(f)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to carry %q", err, tc.want)
			}
		})
	}
}

func TestReviewRunFlagRefusalNamesEveryInertKnob(t *testing.T) {
	f, err := parseReviewRun(t, "--base", "b", "--notes-model", "n", "--chunk-size", "2")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, _, err = reviewRunOptions(f)
	if err == nil {
		t.Fatal("reviewRunOptions accepted maps knobs on single-pass")
	}
	for _, want := range []string{"--chunk-size", "--notes-model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
}

func TestReviewRunMissingDescriptionFile(t *testing.T) {
	f, err := parseReviewRun(t, "--base", "b", "--description-file", filepath.Join(t.TempDir(), "nope.md"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, err := reviewRunOptions(f); err == nil || !strings.Contains(err.Error(), "nope.md") {
		t.Fatalf("error = %v, want the missing description file named", err)
	}
}

func TestReviewRunHelpSucceeds(t *testing.T) {
	_, err := parseReviewRun(t, "--help")
	if !errors.Is(err, pflag.ErrHelp) {
		t.Fatalf("error = %v, want pflag.ErrHelp (help exits 0)", err)
	}
}

func TestReviewRunUnknownFlagFails(t *testing.T) {
	if _, err := parseReviewRun(t, "--base", "b", "--methd", "single"); err == nil {
		t.Fatal("parse accepted an unknown flag")
	}
}

func TestWriteReviewReport(t *testing.T) {
	report := &loop.ReviewRunReport{
		Base: "b", Head: "h", Method: config.ReviewMethodSinglePass,
		Models:   map[string]string{"review": "m"},
		Findings: []config.DoneFinding{}, NotesTurns: []string{},
	}
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReviewReport(path, report, &stdout, &stderr); err != nil {
		t.Fatalf("writeReviewReport: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing when --json names a file", stdout.String())
	}
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("stderr = %q, want the report path", stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got loop.ReviewRunReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the written report does not parse: %v\n%s", err, data)
	}
	if got.Head != "h" || got.Findings == nil {
		t.Errorf("written report = %+v, want the run's report", got)
	}

	stdout.Reset()
	if err := writeReviewReport("", report, &stdout, &stderr); err != nil {
		t.Fatalf("writeReviewReport to stdout: %v", err)
	}
	if !strings.Contains(stdout.String(), `"head": "h"`) {
		t.Errorf("stdout = %q, want the JSON document", stdout.String())
	}
	if err := writeReviewReport("", nil, &stdout, &stderr); err != nil {
		t.Fatalf("writeReviewReport(nil): %v", err)
	}
}
