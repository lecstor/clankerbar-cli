// Package release decides what a `staging -> main` promotion should publish:
// the next semver tag its conventional commits imply, and whether the required
// `ci` check on the merge commit actually passed.
//
// Both halves exist because the alternative is a human cutting releases by hand,
// which kept losing the race with merges - `main` moved ahead of the released
// binary twice in one session on 2026-08-10, and a stale binary is silent.
//
// The two rules worth knowing before reading the code, because both are
// deliberate and both look like bugs otherwise:
//
//   - An UNRECOGNISED commit subject bumps PATCH, it does not bump nothing.
//     A commit nobody can classify is far more likely to be a real change written
//     in a hurry than a no-op, and the failure this package exists to prevent is
//     a change that ships nothing and says nothing about it.
//   - While the major version is 0, a breaking marker bumps MINOR, never to
//     v1.0.0. 1.0.0 is a statement about stability that a maintainer makes, not a
//     mechanical consequence of someone typing `fix(config)!:` - which this repo
//     has already done, at 0.x, without meaning 1.0.
package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Bump is how far a version moves. The values are ordered so that a range of
// commits bumps by the MAXIMUM its members imply - that ordering is load-bearing
// and Max below depends on it.
type Bump int

const (
	// BumpNone is a commit that ships nothing a user could observe (docs, chores,
	// tests, CI config). A promotion made entirely of these publishes no release.
	BumpNone Bump = iota
	BumpPatch
	BumpMinor
	BumpMajor
)

func (b Bump) String() string {
	switch b {
	case BumpNone:
		return "none"
	case BumpPatch:
		return "patch"
	case BumpMinor:
		return "minor"
	case BumpMajor:
		return "major"
	default:
		return fmt.Sprintf("Bump(%d)", int(b))
	}
}

// Max returns the larger of two bumps - a range takes the strongest bump any one
// of its commits asks for.
func Max(a, b Bump) Bump {
	if a > b {
		return a
	}
	return b
}

// subjectRe matches the conventional-commit prefix of a subject line: a type, an
// optional (scope), an optional `!` breaking marker, then the colon. The type is
// letters only, so a subject like `WIP: something` (type `WIP`) still parses and
// falls through to the unrecognised-type branch rather than being dropped.
var subjectRe = regexp.MustCompile(`^([a-zA-Z]+)(\(([^)]*)\))?(!)?:`)

// breakingFooterRe matches the conventional-commits breaking-change footer, in
// both spellings the spec allows. Anchored per-line: `BREAKING CHANGE:` quoted
// mid-paragraph (a commit body DISCUSSING one) must not trip it.
var breakingFooterRe = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE:`)

// bumpByType is the type -> bump mapping. A type absent from this map is
// UNRECOGNISED and bumps patch (see the package doc); it is not silently none.
// Only types that demonstrably ship nothing to a user of the binary map to
// BumpNone, and `review` is in that set because it is this project's own
// review-cycle chatter - the same list .goreleaser.yaml filters out of the
// changelog, kept in step with it deliberately.
var bumpByType = map[string]Bump{
	"feat":     BumpMinor,
	"fix":      BumpPatch,
	"perf":     BumpPatch,
	"revert":   BumpPatch,
	"docs":     BumpNone,
	"chore":    BumpNone,
	"test":     BumpNone,
	"refactor": BumpNone,
	"style":    BumpNone,
	"ci":       BumpNone,
	"build":    BumpNone,
	"review":   BumpNone,
}

// Subject returns a commit message's subject line - the first NON-EMPTY line.
//
// It exists as its own function, rather than as an inline `Cut(message, "\n")`,
// because of a real bug it was written to fix. The workflow feeds commits in as
// `git log --no-merges --format=%B%x00`, and git separates the RECORDS with a
// newline: every message after the first therefore arrives with a leading "\n".
// Taking the literal first line gave an empty subject, which matched no
// conventional-commit prefix, which sent every such commit down the
// unrecognised->patch branch. The result was a parser that quietly derived patch
// bumps forever and never a minor - the exact silent mis-versioning this package
// is supposed to prevent, and it was invisible until the derivation was run
// against real history.
//
// Skipping leading blank lines is therefore not defensive tidying; it is the fix.
func Subject(message string) string {
	for line := range strings.SplitSeq(message, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

// BumpFor returns the bump a single commit message implies. It takes the WHOLE
// message, not just the subject, because the breaking-change footer lives in the
// body and a `feat` carrying one is a major, not a minor.
//
// An empty or whitespace-only message bumps nothing: there is no commit there to
// classify, and treating it as an unrecognised change would let a stray blank
// record force a release.
func BumpFor(message string) Bump {
	subject := Subject(message)
	if subject == "" {
		return BumpNone
	}

	m := subjectRe.FindStringSubmatch(subject)
	if m == nil {
		// Not a conventional subject at all. Unrecognised, so: patch.
		return BumpPatch
	}

	// The `!` marker and the footer are equivalent under the spec, and either
	// outranks whatever the type would otherwise have implied.
	if m[4] == "!" || breakingFooterRe.MatchString(message) {
		return BumpMajor
	}

	if b, ok := bumpByType[strings.ToLower(m[1])]; ok {
		return b
	}
	// A well-formed subject with a type we do not know. Same reasoning as an
	// unparseable one: classify it as a change, not as nothing.
	return BumpPatch
}

// BumpForAll returns the bump a whole range of commits implies - the strongest
// any single commit asks for. An empty range bumps nothing.
func BumpForAll(messages []string) Bump {
	out := BumpNone
	for _, m := range messages {
		out = Max(out, BumpFor(m))
	}
	return out
}

// Version is a plain semver release version. Prerelease and build metadata are
// deliberately absent: this package only ever DERIVES ordinary releases, and an
// existing prerelease tag is something it refuses to reason from rather than
// guesses at (see ParseVersion).
type Version struct {
	Major, Minor, Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseVersion reads a `vX.Y.Z` (or `X.Y.Z`) tag.
//
// It REFUSES a prerelease or build-metadata suffix rather than stripping it. The
// caller is asking "what comes after this release?", and the honest answer from
// `v0.4.0-rc1` is not derivable without knowing whether the rc shipped - so this
// errors, the workflow goes red, and a human picks. A wrong guess here publishes
// a version number that cannot be taken back.
func ParseVersion(tag string) (Version, error) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, fmt.Errorf("empty version tag")
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		return Version{}, fmt.Errorf("version %q carries a prerelease/build suffix; refusing to derive from it", tag)
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q is not vX.Y.Z", tag)
	}
	var out [3]int
	for i, p := range parts {
		if p == "" {
			return Version{}, fmt.Errorf("version %q is not vX.Y.Z", tag)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("version %q is not vX.Y.Z", tag)
		}
		out[i] = n
	}
	return Version{Major: out[0], Minor: out[1], Patch: out[2]}, nil
}

// Next applies a bump. The second return is whether anything is to be released
// at all - false for BumpNone, and the caller must then publish NOTHING rather
// than tagging an identical version.
//
// The pre-1.0 clause: at major 0, a BumpMajor becomes a minor bump. See the
// package doc for why that is a policy choice and not an oversight.
func Next(current Version, b Bump) (Version, bool) {
	switch b {
	case BumpNone:
		return current, false
	case BumpMajor:
		if current.Major == 0 {
			return Version{Major: 0, Minor: current.Minor + 1, Patch: 0}, true
		}
		return Version{Major: current.Major + 1, Minor: 0, Patch: 0}, true
	case BumpMinor:
		return Version{Major: current.Major, Minor: current.Minor + 1, Patch: 0}, true
	case BumpPatch:
		return Version{Major: current.Major, Minor: current.Minor, Patch: current.Patch + 1}, true
	default:
		return current, false
	}
}

// Derivation is what NextFromCommits worked out, in the shape the workflow
// consumes it.
type Derivation struct {
	// Current is the tag derivation started from.
	Current Version
	// Next is the tag to publish. Meaningful only when Release is true.
	Next Version
	// Bump is what the commits implied, BEFORE the pre-1.0 clause is applied -
	// so a major bump held down to a minor still reports "major" here, and the
	// workflow log says what the commits actually asked for.
	Bump Bump
	// Release is false when the range carries nothing publishable. The caller
	// skips quietly: no tag, no release, exit 0.
	Release bool
}

// NextFromCommits is the whole derivation in one call: the newest release tag
// plus the messages of the non-merge commits since it, giving the tag to publish.
//
// Merge commits must already be excluded by the caller (`git log --no-merges`).
// This repo merges every task PR as a merge commit, so the conventional-commit
// prefixes live on the INNER commits - reading first-parent subjects instead
// would see nothing but `Merge pull request #NN` and derive a patch bump forever.
func NextFromCommits(currentTag string, messages []string) (Derivation, error) {
	cur, err := ParseVersion(currentTag)
	if err != nil {
		return Derivation{}, err
	}
	b := BumpForAll(messages)
	next, release := Next(cur, b)
	return Derivation{Current: cur, Next: next, Bump: b, Release: release}, nil
}
