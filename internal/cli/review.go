package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

// review.go — `clankerbar review run` (CLA-870): the standalone entry point to
// the driver's review, outside any task, claim or plane. It exists so an eval
// harness scores the review the driver really runs, and so a human can aim the
// same review at any local base..head.
//
// The command owns only the flag vocabulary and the JSON document; the review
// itself is loop.RunReview. That split is deliberate: the brief, the maps and
// the report parsing are the review phase's, not a second implementation.

type reviewRunFlags struct {
	repo, base, head                 string
	method, harness                  string
	model, notesModel, findingsModel string
	chunkSize, flowMax               int
	jsonOut, title, descriptionFile  string
}

func newReviewRunFlagSet(f *reviewRunFlags) *pflag.FlagSet {
	fs := newFlagSet("review run")
	fs.StringVar(&f.repo, "repo", ".", "local git checkout to review (the repository is never modified; the review runs in a temporary worktree)")
	fs.StringVar(&f.base, "base", "", "commit the review diffs from; required")
	fs.StringVar(&f.head, "head", "HEAD", "commit the review diffs to")
	fs.StringVar(&f.method, "method", config.ReviewMethodSinglePass, "review method: \"single-pass\" (or \"single\") or \"maps-session\"")
	fs.StringVar(&f.harness, "harness", "opencode", "harness to run the review session on")
	fs.StringVar(&f.model, "model", "", "model to review on; empty runs the harness's default (maps-session: the phase's model)")
	fs.StringVar(&f.notesModel, "notes-model", "", "maps-session only: model for the notes turns (empty inherits --model)")
	fs.StringVar(&f.findingsModel, "findings-model", "", "maps-session only: model for the findings turn (empty inherits --model, else the notes model)")
	fs.IntVar(&f.chunkSize, "chunk-size", 0, fmt.Sprintf("maps-session only: map items per notes turn (0 = %d)", config.DefaultMapsSessionChunkSize))
	fs.IntVar(&f.flowMax, "flow-max", 0, fmt.Sprintf("maps-session only: cap on the flow map's items (0 = %d)", config.DefaultMapsSessionFlowMax))
	fs.StringVar(&f.jsonOut, "json", "", "write the findings JSON to this file instead of stdout")
	fs.StringVar(&f.title, "title", "", "optional title of the change, shown to the reviewer")
	fs.StringVar(&f.descriptionFile, "description-file", "", "optional file whose contents describe the change, shown to the reviewer")
	return fs
}

// Review is the `clankerbar review` entry point. `run` is its only subcommand.
func Review(ctx context.Context, args []string) error {
	return reviewRun(ctx, args, os.Stdout, os.Stderr)
}

func reviewRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			reviewUsage(stdout)
			return nil
		case "run":
		default:
			reviewUsage(stderr)
			return fmt.Errorf("unknown review subcommand %q: the command is `clankerbar review run`", args[0])
		}
	} else {
		reviewUsage(stderr)
		return errors.New("clankerbar review needs a subcommand: `clankerbar review run --base <commit>`")
	}

	var f reviewRunFlags
	fs := newReviewRunFlagSet(&f)
	if err := parseFlags(fs, args[1:]); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	opts, jsonPath, err := reviewRunOptions(f)
	if err != nil {
		return err
	}
	opts.Console = stderr

	report, runErr := loop.RunReview(ctx, opts)
	writeErr := writeReviewReport(jsonPath, report, stdout, stderr)
	if runErr != nil && writeErr != nil {
		return errors.Join(runErr, writeErr)
	}
	if runErr != nil {
		return runErr
	}
	return writeErr
}

// reviewRunOptions validates one parsed `review run` invocation and maps it onto
// the loop options. Validation that would spend a session is done here, so a
// typo costs nothing.
func reviewRunOptions(f reviewRunFlags) (loop.ReviewRunOptions, string, error) {
	method, err := loop.NormalizeReviewMethod(f.method)
	if err != nil {
		return loop.ReviewRunOptions{}, "", err
	}
	if strings.TrimSpace(f.base) == "" {
		return loop.ReviewRunOptions{}, "", errors.New("--base is required: name the commit the review diffs from")
	}
	if strings.TrimSpace(f.harness) == "" {
		return loop.ReviewRunOptions{}, "", errors.New("--harness must name a harness")
	}
	if method != config.ReviewMethodMapsSession {
		var knobs []string
		for name, set := range map[string]bool{
			"--notes-model":    strings.TrimSpace(f.notesModel) != "",
			"--findings-model": strings.TrimSpace(f.findingsModel) != "",
			"--chunk-size":     f.chunkSize != 0,
			"--flow-max":       f.flowMax != 0,
		} {
			if set {
				knobs = append(knobs, name)
			}
		}
		if len(knobs) > 0 {
			sort.Strings(knobs)
			return loop.ReviewRunOptions{}, "", fmt.Errorf("%s only applies to --method %q, and the method is %q: set it or drop the flag",
				strings.Join(knobs, ", "), config.ReviewMethodMapsSession, method)
		}
	}
	if f.chunkSize < 0 {
		return loop.ReviewRunOptions{}, "", fmt.Errorf("--chunk-size must not be negative, got %d", f.chunkSize)
	}
	if f.flowMax < 0 {
		return loop.ReviewRunOptions{}, "", fmt.Errorf("--flow-max must not be negative, got %d", f.flowMax)
	}

	description := ""
	if path := strings.TrimSpace(f.descriptionFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return loop.ReviewRunOptions{}, "", fileError(path, err)
		}
		description = string(data)
	}

	return loop.ReviewRunOptions{
		Repo:          f.repo,
		Base:          f.base,
		Head:          f.head,
		Method:        method,
		Harness:       f.harness,
		Model:         f.model,
		NotesModel:    f.notesModel,
		FindingsModel: f.findingsModel,
		ChunkSize:     f.chunkSize,
		FlowMax:       f.flowMax,
		Title:         f.title,
		Description:   description,
	}, f.jsonOut, nil
}

// writeReviewReport writes the JSON document, even when the run itself failed:
// the report carries what the session did produce, and losing it would make an
// eval's failure unreadable. A nil report means the run failed before a session
// was composed, and there is nothing to write.
func writeReviewReport(path string, report *loop.ReviewRunReport, stdout, stderr io.Writer) error {
	if report == nil {
		return nil
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the review report: %w", err)
	}
	data = append(data, '\n')
	if path != "" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write the review report to %s: %w", path, err)
		}
		fmt.Fprintf(stderr, "review report written to %s\n", path)
		return nil
	}
	_, err = stdout.Write(data)
	return err
}

// reviewUsage is the `clankerbar review` help.
func reviewUsage(w io.Writer) {
	fmt.Fprint(w, `clankerbar review — run the driver's review outside the backlog.

Usage:
  clankerbar review run [flags]

"review run" reviews one local base..head in a temporary detached worktree and
prints the findings as JSON. It claims no task, writes no plane record and
connects no MCP config: the change itself is the whole subject, and the JSON is
the whole deliverable. The reviewed repository is only read from; nothing is
edited, committed or pushed.

  --repo <path>             local git checkout (default: .)
  --base <commit>           commit the review diffs from (required)
  --head <commit>           commit the review diffs to (default: HEAD)
  --method <method>         "single-pass" (or "single") or "maps-session"
                            (default: single-pass)
  --harness <name>          harness to run on (default: opencode)
  --model <alias>           model to review on (empty: the harness's default)
  --notes-model <alias>     maps-session: model for the notes turns
  --findings-model <alias>  maps-session: model for the findings turn
  --chunk-size <n>          maps-session: map items per notes turn (default: 25)
  --flow-max <n>            maps-session: flow-map item cap (default: 40)
  --title <text>            optional title of the change
  --description-file <f>    optional file describing the change
  --json <file>             write the JSON to a file (default: stdout)

The JSON carries the resolved base/head commits, the method, the model per
stage, the findings, the omitted count, the maps-session notes and the session's
usage. A session that ends without a usable done report fails the command
(non-zero), after the harness's one re-emit.
`)
}
