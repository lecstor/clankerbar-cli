package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// upload and deck (CLA-581): the command-line half of the byte-free upload
// path. An agent (or a human) runs `clankerbar upload shot.png` and gets back a
// reference to paste, instead of computing a sha256 and assembling curl by
// hand. The bytes are sent by this process from the file on disk; neither the
// MCP call nor anything upstream ever carries them.
//
// The stdout contract is the compose-in-a-script one: `upload` prints exactly
// `asset:<id>` and `deck` exactly the review URL, so `IMG=$(clankerbar upload
// shot.png)` yields a usable reference. Human-facing lines go to stderr, so
// they never contaminate the captured value.

type uploadFlags struct {
	cfgPath string
	project string
	task    string
	mime    string
}

func newUploadFlagSet(f *uploadFlags) *pflag.FlagSet {
	fs := newFlagSet("upload")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file naming the plane and project (default: ~/.config/clankerbar/config.json)")
	fs.StringVar(&f.project, "project", "", "clankerbar project slug to upload to (default: the config's project)")
	fs.StringVar(&f.task, "task", "", "task this file belongs to (a CLA-12 ref or id) - optional attribution")
	fs.StringVar(&f.mime, "type", "", "content type, when the file's extension does not say (e.g. image/png)")
	return fs
}

type deckFlags struct {
	cfgPath string
	project string
	task    string
}

func newDeckFlagSet(f *deckFlags) *pflag.FlagSet {
	fs := newFlagSet("deck")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file naming the plane and project (default: ~/.config/clankerbar/config.json)")
	fs.StringVar(&f.project, "project", "", "clankerbar project slug (default: the config's project)")
	fs.StringVar(&f.task, "task", "", "the task this deck reviews (a CLA-12 ref or id); required")
	return fs
}

// contentTypeByExtension is the extension table for the types the plane's asset
// store accepts, and nothing else. It is deliberately explicit rather than
// mime.TypeByExtension: that reads host mime databases, so the same file would
// declare a different content type (or none) on another machine, and an
// agent-facing command needs the same answer everywhere. An extension that is
// not here needs --type; a type the plane does not allow is refused there, in
// the plane's words, and is not pre-judged here.
var contentTypeByExtension = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".svg":  "image/svg+xml",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".html": "text/html",
	".htm":  "text/html",
}

// contentTypeFor reports the content type the file's extension implies.
func contentTypeFor(path string) (string, bool) {
	t, ok := contentTypeByExtension[strings.ToLower(filepath.Ext(path))]
	return t, ok
}

// checkReadable refuses a path that is missing or not a regular file, without
// reading a byte of it. Both commands run this BEFORE their extension checks so
// an unreadable file is reported as exactly that, rather than as a content-type
// problem that --type cannot fix.
func checkReadable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fileError(path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return nil
}

// fileError renders a filesystem failure with the path quoted. The operating
// system's own rendering embeds the raw path (`open <path>: ...`), so a path
// containing a newline would otherwise split a one-line stderr reason across
// two lines; rebuilding the message from the wrapped cause is what makes the
// quoted form the whole path, and it keeps errors.Is working against the cause.
func fileError(path string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s %q: %w", pe.Op, path, pe.Err)
	}
	return fmt.Errorf("%q: %w", path, err)
}

// hashFile returns the file's sha256 (lowercase hex) and size, streaming it so
// a large file is never held in memory. The plane verifies both against the
// bytes the PUT delivers, so they are the declaration's whole substance.
func hashFile(path string) (string, int64, error) {
	if err := checkReadable(path); err != nil {
		return "", 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fileError(path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fileError(path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// parseOneFileArg parses upload/deck args. Unlike most subcommands these take
// exactly one POSITIONAL — the file — so parseFlags' no-positionals rule does
// not apply; this is that function with the rule replaced (the shape parseCtlArgs
// already uses for ctl's action).
func parseOneFileArg(fs *pflag.FlagSet, args []string, want string) (string, error) {
	if err := rejectSingleDashLongFlags(fs, args); err != nil {
		printUsage(fs)
		return "", err
	}
	if err := fs.Parse(args); err != nil {
		printUsage(fs)
		return "", err
	}
	if helpRequested(fs) {
		// Same convention as parseFlags: help SUCCEEDED, so stdout.
		if fs.Output() == os.Stderr {
			fs.SetOutput(os.Stdout)
			defer fs.SetOutput(os.Stderr)
		}
		printUsage(fs)
		return "", pflag.ErrHelp
	}
	if fs.NArg() != 1 {
		printUsage(fs)
		return "", errors.New(want)
	}
	return fs.Arg(0), nil
}

// uploadAPI resolves the config, the target project and the account key, and
// returns the upload surface pointed at that project's MCP endpoint.
//
// The config is loaded and VALIDATED the way the daemon does it, because
// Validate is what pins the workdir absolutely and discovers `<workdir>/.mcp.json`
// — the file whose `/mcp/<slug>` names the project in the single-project case —
// so a command that skipped it could resolve a different project than the
// daemon's own poll. The endpoint itself is still assembled only from
// CredentialOrigin (never the workdir file's host), exactly like every other
// credentialed call.
func uploadAPI(cfgPath, project string) (plane.UploadAPI, error) {
	key := os.Getenv("CLANKERBAR_API_KEY")
	if key == "" {
		return nil, errors.New("CLANKERBAR_API_KEY is not set - export the account key the daemon runs with")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	slug := project
	if slug == "" {
		// No --project: the config answers, the same derivation the daemon's
		// poll uses. A multi-project config has no single answer and says so,
		// naming --project as the way to choose.
		slug, err = pickSlug(cfg, "", "--project")
		if err != nil {
			return nil, err
		}
	}

	endpoint := cfg.BacklogEndpoint()
	if slug != "" {
		// ProjectEndpoint is origin + /mcp/<slug>: the slug is path-escaped and
		// the origin is CredentialOrigin, never a host read off a checkout.
		endpoint = cfg.ProjectEndpoint(config.Project{Slug: slug})
	}
	if endpoint == "" {
		return nil, errors.New("cannot resolve a project-scoped MCP endpoint - pass --project <slug>, or set backlog_url in the config")
	}
	return plane.NewUploadAPI(endpoint, key), nil
}

// Upload is `clankerbar upload <file>`: declare the file, send its bytes, print
// `asset:<id>` on stdout.
func Upload(ctx context.Context, args []string) error {
	return uploadRun(ctx, args, os.Stdout, os.Stderr)
}

func uploadRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f uploadFlags
	fs := newUploadFlagSet(&f)
	path, err := parseOneFileArg(fs, args, "clankerbar upload needs exactly one file: upload <file> [--task <ref|id>] [--type <mime>]")
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	if err := checkReadable(path); err != nil {
		return err
	}

	contentType := strings.TrimSpace(f.mime)
	if contentType == "" {
		detected, ok := contentTypeFor(path)
		if !ok {
			return fmt.Errorf("cannot tell the content type of %q from its extension - pass --type <mime> (e.g. --type image/png)", path)
		}
		contentType = detected
	}
	contentType = strings.ToLower(contentType)

	sha, size, err := hashFile(path)
	if err != nil {
		return err
	}

	api, err := uploadAPI(f.cfgPath, f.project)
	if err != nil {
		return err
	}

	assetID, err := declareAndSend(ctx, api, path, contentType, sha, size, f.task, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "asset:%s\n", assetID)
	return nil
}

// declareAndSend runs the shared two-step upload: declare (create_upload), then
// PUT the bytes unless the plane said identical bytes are already stored. The
// human progress line goes to stderr and the asset id is returned, so both
// commands keep stdout for the one machine-readable reference.
func declareAndSend(ctx context.Context, api plane.UploadAPI, path, contentType, sha string, size int64, taskID string, stderr io.Writer) (string, error) {
	ticket, err := api.CreateUpload(ctx, plane.CreateUploadRequest{
		ContentType: contentType,
		SizeBytes:   size,
		SHA256:      sha,
		TaskID:      taskID,
	})
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	if ticket.AlreadyStored {
		fmt.Fprintf(stderr, "%q is already stored in this project (%q, %d bytes) - reusing asset:%s\n",
			path, contentType, size, ticket.AssetID)
		return ticket.AssetID, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fileError(path, err)
	}
	defer f.Close()
	if err := api.PutUpload(ctx, ticket.UploadURL, f, size, contentType); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	fmt.Fprintf(stderr, "uploaded %q (%q, %d bytes) as asset:%s\n", path, contentType, size, ticket.AssetID)
	return ticket.AssetID, nil
}

// Deck is `clankerbar deck <file.html> --task <ref|id>`: upload the HTML as an
// asset, bind it to the task as its review deck, print the review URL on stdout.
func Deck(ctx context.Context, args []string) error {
	return deckRun(ctx, args, os.Stdout, os.Stderr)
}

func deckRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f deckFlags
	fs := newDeckFlagSet(&f)
	path, err := parseOneFileArg(fs, args, "clankerbar deck needs exactly one file: deck <file.html> --task <ref|id>")
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(f.task) == "" {
		return errors.New("--task is required: a deck reviews one task, so the task it binds to must be named")
	}
	if err := checkReadable(path); err != nil {
		return err
	}
	// A deck is HTML by definition; `upload_review_deck` only serves a ready
	// text/html asset, so anything else is refused here rather than declared as
	// HTML and refused there. .html/.htm is the only spelling accepted.
	if t, ok := contentTypeFor(path); !ok || t != "text/html" {
		return fmt.Errorf("deck expects an HTML file (a .html or .htm file): %q is not one", path)
	}

	sha, size, err := hashFile(path)
	if err != nil {
		return err
	}
	api, err := uploadAPI(f.cfgPath, f.project)
	if err != nil {
		return err
	}
	assetID, err := declareAndSend(ctx, api, path, "text/html", sha, size, f.task, stderr)
	if err != nil {
		return err
	}
	reviewURL, err := api.UploadReviewDeck(ctx, f.task, assetID)
	if err != nil {
		return fmt.Errorf("deck: %w", err)
	}
	fmt.Fprintf(stderr, "deck for %q is live (asset:%s)\n", f.task, assetID)
	fmt.Fprintln(stdout, reviewURL)
	return nil
}
