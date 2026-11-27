// Package auth owns the operator's LOCAL credential store: the per-origin API
// keys `clankerbar login` writes once per machine and every command reads in
// place of an exported CLANKERBAR_API_KEY.
//
// The store is one owner-only JSON file at <XDG_STATE_HOME>/clankerbar/auth.json
// (see Path). One file rather than per-command wiring is the point: a GUI or TUI
// harness that never inherits a shell's environment cannot be handed an exported
// variable, so the credential has to live somewhere the CLI itself can find.
//
// The rules that make holding a credential in a file acceptable are enforced
// here, not at each call site:
//
//   - the file is owner-only. An existing file readable by group or other, or
//     owned by another user, is REFUSED with the chmod that fixes it.
//   - the directory is created 0700, and a symlink at the file or any directory
//     component below the deepest existing ancestor is refused.
//   - writes are atomic (a same-directory temp file renamed over the target), so
//     a concurrent reader never sees half a JSON document.
//   - a token is never put into an error string or any output: errors name paths,
//     origins and modes, never values.
//
// CLANKERBAR_AUTH_FILE relocates the file (tests, per-profile setups); set to
// "none" or the platform's null device it DISABLES stored credentials entirely,
// so an isolated session can be told to trust only its own environment.
package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

const (
	// EnvAPIKey is the account-wide key every command already understood. It
	// keeps precedence over the store: an explicit export is the operator saying
	// "this invocation, this key".
	EnvAPIKey = "CLANKERBAR_API_KEY"

	// EnvAuthFile relocates the credential store. "none" (or os.DevNull)
	// disables stored credentials: reads answer "not found" without touching
	// disk, which is how a review or unattended session is isolated from the
	// operator's ambient login.
	EnvAuthFile = "CLANKERBAR_AUTH_FILE"

	// DefaultOrigin is where the CLI authenticates when nothing else names a
	// plane. It is the same built-in default config uses for backlog_url.
	DefaultOrigin = "https://clankerbar.com"

	// FileVersion is the schema version written to auth.json. A version this
	// build does not know is refused rather than guessed at.
	FileVersion = 1

	// ScopeUser is an account-wide key: every project the operator can see.
	ScopeUser = "user"
	// ScopeProject is a key bound to one project (the device flow's project
	// approval).
	ScopeProject = "project"
)

// Credential is one stored origin's entry.
type Credential struct {
	Token       string    `json:"token"`
	Scope       string    `json:"scope,omitempty"`
	ProjectSlug string    `json:"project_slug,omitempty"`
	SavedAt     time.Time `json:"saved_at"`
}

// String redacts the token, so an accidentally logged Credential cannot leak it.
func (c Credential) String() string {
	return fmt.Sprintf("credential{scope:%q project:%q token:redacted}", c.Scope, c.ProjectSlug)
}

// Store is the whole auth.json document.
type Store struct {
	Version int                   `json:"version"`
	Hosts   map[string]Credential `json:"hosts"`
}

// String redacts every token, like Credential.
func (s Store) String() string {
	return fmt.Sprintf("auth store{version:%d hosts:%d}", s.Version, len(s.Hosts))
}

// Path returns the credential file's absolute-or-configured path and whether
// stored credentials are enabled. A disabled store ("none"/os.DevNull) answers
// ("", false, nil): callers must not touch the filesystem at all.
func Path() (string, bool, error) {
	if v := strings.TrimSpace(os.Getenv(EnvAuthFile)); v != "" {
		if isDisabledPath(v) {
			return "", false, nil
		}
		return expandHome(v), true, nil
	}
	home, err := config.StateHome()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(home, "clankerbar", "auth.json"), true, nil
}

// isDisabledPath reports whether a CLANKERBAR_AUTH_FILE value means "no store".
// Case-insensitive because an operator writing NONE means the same thing as one
// writing none, and both are decisions, not paths.
func isDisabledPath(v string) bool {
	if strings.EqualFold(v, "none") {
		return true
	}
	return filepath.Clean(v) == filepath.Clean(os.DevNull)
}

// expandHome expands a leading ~ (Go does not do this for us).
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// CanonicalOrigin normalises and validates origin with the same rule every
// credentialed request uses: https anywhere, http only to loopback.
func CanonicalOrigin(origin string) (string, error) {
	return canonicalOrigin(origin)
}

// ErrNotAuthenticated is returned by Resolve when neither the environment nor
// the store has a credential for the origin. Callers turn it into a message
// naming `clankerbar login`.
var ErrNotAuthenticated = errors.New("no credential for this origin")

// ErrStoreDisabled is returned when a WRITE is attempted while
// CLANKERBAR_AUTH_FILE disables the store.
var ErrStoreDisabled = errors.New("stored credentials are disabled by " + EnvAuthFile)
