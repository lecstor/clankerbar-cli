package cli

// review_map.go — `clankerbar review map` (CLA-975): print the plan
// `review run --method maps-session` would send, as JSON, starting no session.
// The plan is loop.ReviewMap, which builds through the same function as
// `review run`, so the CLI layer here is only the flag vocabulary and the JSON
// write.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

type reviewMapFlags struct {
	repo, base, head                string
	chunkSize, flowMax              int
	jsonOut, title, descriptionFile string
}

func newReviewMapFlagSet(f *reviewMapFlags) *pflag.FlagSet {
	fs := newFlagSet("review map")
	fs.StringVar(&f.repo, "repo", ".", "local git checkout to map (it must be checked out at --head; the repository is never modified)")
	fs.StringVar(&f.base, "base", "", "commit the review diffs from; required")
	fs.StringVar(&f.head, "head", "HEAD", "commit the review diffs to (the checkout must be at it)")
	fs.IntVar(&f.chunkSize, "chunk-size", 0, fmt.Sprintf("map items per notes turn (0 = %d)", config.DefaultMapsSessionChunkSize))
	fs.IntVar(&f.flowMax, "flow-max", 0, fmt.Sprintf("cap on the flow map's items (0 = %d)", config.DefaultMapsSessionFlowMax))
	fs.StringVar(&f.jsonOut, "json", "", "write the plan JSON to this file instead of stdout")
	fs.StringVar(&f.title, "title", "", "optional title of the change, shown to the reviewer")
	fs.StringVar(&f.descriptionFile, "description-file", "", "optional file whose contents describe the change, shown to the reviewer")
	return fs
}

// reviewMap is the `clankerbar review map` handler. It starts no session and
// reads no config: the JSON is the whole deliverable, and progress goes to
// stderr so a stdout consumer gets only the document.
func reviewMap(args []string, stdout, stderr io.Writer) error {
	var f reviewMapFlags
	fs := newReviewMapFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	opts, jsonPath, err := reviewMapOptions(f)
	if err != nil {
		return err
	}
	report, err := loop.ReviewMap(opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "review map: %d change items, %d flow items, %d turns: %s..%s\n",
		report.Items.Change, report.Items.Flow, len(report.Turns), report.Base, report.Head)
	return writeReviewMapReport(jsonPath, report, stdout, stderr)
}

// reviewMapOptions validates one parsed `review map` invocation. It mirrors
// `review run`'s flag names, defaults and validation for the flags both take.
func reviewMapOptions(f reviewMapFlags) (loop.ReviewMapOptions, string, error) {
	if strings.TrimSpace(f.base) == "" {
		return loop.ReviewMapOptions{}, "", errors.New("--base is required: name the commit the review diffs from")
	}
	if f.chunkSize < 0 {
		return loop.ReviewMapOptions{}, "", fmt.Errorf("--chunk-size must not be negative, got %d", f.chunkSize)
	}
	if f.flowMax < 0 {
		return loop.ReviewMapOptions{}, "", fmt.Errorf("--flow-max must not be negative, got %d", f.flowMax)
	}

	description := ""
	if path := strings.TrimSpace(f.descriptionFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return loop.ReviewMapOptions{}, "", fileError(path, err)
		}
		description = string(data)
	}

	return loop.ReviewMapOptions{
		Repo:        f.repo,
		Base:        f.base,
		Head:        f.head,
		ChunkSize:   f.chunkSize,
		FlowMax:     f.flowMax,
		Title:       f.title,
		Description: description,
	}, f.jsonOut, nil
}

// writeReviewMapReport writes the plan document. A path wins over stdout; the
// path is progress and goes to stderr.
func writeReviewMapReport(path string, report *loop.ReviewMapReport, stdout, stderr io.Writer) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the review map: %w", err)
	}
	data = append(data, '\n')
	if path != "" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write the review map to %s: %w", path, err)
		}
		fmt.Fprintf(stderr, "review map written to %s\n", path)
		return nil
	}
	_, err = stdout.Write(data)
	return err
}
