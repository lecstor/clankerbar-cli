package auth

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Source names where a resolved credential came from.
type Source string

const (
	// SourceEnv is CLANKERBAR_API_KEY in this process's environment.
	SourceEnv Source = "env"
	// SourceFile is the local credential store.
	SourceFile Source = "file"
)

// Resolution is the credential one command should use for one origin, plus the
// facts `whoami` reports about it. Token is the only secret; every consumer
// must keep it off stdout, stderr and error strings.
type Resolution struct {
	Token       string
	Source      Source
	Path        string // the credential file, when Source is SourceFile
	Scope       string
	ProjectSlug string
	SavedAt     time.Time
}

// String redacts the token, so a logged Resolution cannot leak it.
func (r Resolution) String() string {
	return fmt.Sprintf("credential{source:%q path:%q scope:%q token:redacted}", r.Source, r.Path, r.Scope)
}

// Resolve answers which token may be sent to origin, in the one precedence every
// command shares: CLANKERBAR_API_KEY first, then the stored entry for exactly
// that canonical origin. A token stored for origin A is never returned for
// origin B — the map is keyed by the canonical origin, so there is no lookup
// that could confuse two hosts.
//
// A missing credential is not an error; the returned Resolution is empty. A
// credential file that exists but fails its owner-only checks IS an error: the
// caller must not silently fall back to "not logged in" and leave the operator
// believing the store is fine.
func Resolve(origin string) (Resolution, error) {
	norm, err := canonicalOrigin(origin)
	if err != nil {
		return Resolution{}, err
	}
	if key := strings.TrimSpace(os.Getenv(EnvAPIKey)); key != "" {
		return Resolution{Token: key, Source: SourceEnv}, nil
	}
	cred, path, ok, err := Lookup(norm)
	if err != nil {
		return Resolution{}, err
	}
	if !ok {
		return Resolution{}, nil
	}
	return Resolution{
		Token:       cred.Token,
		Source:      SourceFile,
		Path:        path,
		Scope:       cred.Scope,
		ProjectSlug: cred.ProjectSlug,
		SavedAt:     cred.SavedAt,
	}, nil
}
