package release

import "testing"

func TestBumpFor(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    Bump
	}{
		// The types this repo actually uses. All 20 of the last 20 non-merge
		// commits on main carried one of these, which is what makes derivation
		// viable here at all.
		{"feat is a minor", "feat(loop): add a thing", BumpMinor},
		{"feat unscoped", "feat: add a thing", BumpMinor},
		{"fix is a patch", "fix(config): stop doing a thing", BumpPatch},
		{"fix unscoped", "fix: stop doing a thing", BumpPatch},
		{"perf is a patch", "perf(scan): fewer syscalls", BumpPatch},
		{"revert is a patch", "revert: undo the thing", BumpPatch},

		// Types that ship nothing observable. A promotion made only of these
		// publishes NO release - the docs-only case, pinned.
		{"docs is none", "docs: explain the thing", BumpNone},
		{"chore is none", "chore(deps): bump a pin", BumpNone},
		{"test is none", "test(loop): cover the thing", BumpNone},
		{"refactor is none", "refactor: move the thing", BumpNone},
		{"style is none", "style: gofmt", BumpNone},
		{"ci is none", "ci: pin an action", BumpNone},
		{"build is none", "build: tweak goreleaser", BumpNone},
		{"review is none", "review(loop): address findings", BumpNone},

		// The breaking marker outranks the type, in every position.
		{"bang on feat", "feat!: change the flag", BumpMajor},
		{"bang on fix with scope", "fix(config)!: rename the key", BumpMajor},
		{"bang on a none-type", "chore!: drop go 1.25", BumpMajor},
		{"breaking footer", "feat(cli): add flag\n\nBREAKING CHANGE: --old is gone", BumpMajor},
		{"breaking footer hyphenated", "feat(cli): add flag\n\nBREAKING-CHANGE: --old is gone", BumpMajor},

		// A body that DISCUSSES a breaking change is not a footer declaring one.
		// Anchoring the footer regex per-line is what keeps these apart.
		{"breaking mentioned mid-line is not a footer", "fix: note that a BREAKING CHANGE: would be bad", BumpPatch},

		// The deliberate rule: unrecognised means patch, never none. A change
		// nobody classified must not ship silently as nothing.
		{"non-conventional subject", "made the thing faster", BumpPatch},
		{"unknown type", "wip: halfway through", BumpPatch},
		{"type-like but no colon", "feat add a thing", BumpPatch},
		{"merge subject that slipped through", "Merge pull request #33 from lecstor/x", BumpPatch},

		// Case: types are matched case-insensitively, so `Fix:` is still a patch
		// rather than falling into the unrecognised branch by accident.
		{"uppercase type", "Fix: stop doing a thing", BumpPatch},
		{"uppercase feat", "FEAT: add a thing", BumpMinor},

		// Nothing to classify.
		{"empty", "", BumpNone},
		{"whitespace only", "   \n\t ", BumpNone},

		// REGRESSION. `git log --format=%B%x00` separates records with a newline,
		// so every message after the first arrives with a leading "\n". Reading the
		// literal first line gave "" -> unrecognised -> patch, for EVERY commit,
		// which silently capped derivation at patch forever. Caught only by running
		// the parser over real history.
		{"leading newline from git's record separator", "\nfeat(loop): add a thing", BumpMinor},
		{"several leading blank lines", "\n\n\nfix: stop a thing", BumpPatch},
		{"leading newline with a breaking marker", "\nfeat(config)!: rename a key", BumpMajor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BumpFor(tt.message); got != tt.want {
				t.Errorf("BumpFor(%q) = %v, want %v", tt.message, got, tt.want)
			}
		})
	}
}

func TestBumpForAll_TakesTheStrongest(t *testing.T) {
	tests := []struct {
		name     string
		messages []string
		want     Bump
	}{
		{"empty range", nil, BumpNone},
		{"docs only", []string{"docs: a", "chore: b"}, BumpNone},
		{"a fix among chores", []string{"chore: a", "fix: b", "docs: c"}, BumpPatch},
		{"a feat outranks fixes", []string{"fix: a", "feat: b", "fix: c"}, BumpMinor},
		{"a breaking outranks a feat", []string{"feat: a", "fix(x)!: b"}, BumpMajor},
		{"order does not matter", []string{"fix(x)!: b", "feat: a"}, BumpMajor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BumpForAll(tt.messages); got != tt.want {
				t.Errorf("BumpForAll(%v) = %v, want %v", tt.messages, got, tt.want)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	ok := []struct {
		in   string
		want Version
	}{
		{"v0.3.4", Version{0, 3, 4}},
		{"0.3.4", Version{0, 3, 4}},
		{"v1.0.0", Version{1, 0, 0}},
		{"v10.20.30", Version{10, 20, 30}},
		{"  v0.3.4  ", Version{0, 3, 4}},
	}
	for _, tt := range ok {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseVersion(tt.in)
			if err != nil {
				t.Fatalf("ParseVersion(%q) errored: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}

	bad := []string{
		"",
		"v",
		"v0.3",
		"v0.3.4.5",
		"vX.Y.Z",
		"v0.3.x",
		"v-1.0.0",
		"v0..4",
		// Refused rather than stripped: what follows v0.4.0-rc1 is not derivable
		// without knowing whether the rc shipped, so the workflow must go red.
		"v0.4.0-rc1",
		"v0.4.0+build7",
	}
	for _, in := range bad {
		t.Run("rejects "+in, func(t *testing.T) {
			if _, err := ParseVersion(in); err == nil {
				t.Errorf("ParseVersion(%q) accepted an unparseable tag", in)
			}
		})
	}
}

func TestNext(t *testing.T) {
	tests := []struct {
		name        string
		current     Version
		bump        Bump
		want        Version
		wantRelease bool
	}{
		{"none releases nothing", Version{0, 3, 4}, BumpNone, Version{0, 3, 4}, false},
		{"patch", Version{0, 3, 4}, BumpPatch, Version{0, 3, 5}, true},
		{"minor resets patch", Version{0, 3, 4}, BumpMinor, Version{0, 4, 0}, true},

		// The pre-1.0 clause. At major 0 a breaking change is a MINOR bump: the
		// pipeline must not decide the project has reached 1.0 because someone
		// typed a `!`.
		{"breaking at 0.x bumps minor, not to 1.0.0", Version{0, 3, 4}, BumpMajor, Version{0, 4, 0}, true},
		{"breaking at 0.0.x bumps minor", Version{0, 0, 9}, BumpMajor, Version{0, 1, 0}, true},

		// Past 1.0 it behaves as ordinary semver.
		{"breaking at 1.x bumps major", Version{1, 2, 3}, BumpMajor, Version{2, 0, 0}, true},
		{"minor at 1.x", Version{1, 2, 3}, BumpMinor, Version{1, 3, 0}, true},
		{"patch at 1.x", Version{1, 2, 3}, BumpPatch, Version{1, 2, 4}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, release := Next(tt.current, tt.bump)
			if got != tt.want || release != tt.wantRelease {
				t.Errorf("Next(%v, %v) = %v, %v; want %v, %v",
					tt.current, tt.bump, got, release, tt.want, tt.wantRelease)
			}
		})
	}
}

func TestNextFromCommits(t *testing.T) {
	t.Run("derives the version the commits imply", func(t *testing.T) {
		d, err := NextFromCommits("v0.3.4", []string{
			"fix(loop): stop the thing",
			"docs: explain it",
			"feat(doctor): report the gap",
			"chore: tidy",
		})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if d.Bump != BumpMinor {
			t.Errorf("bump = %v, want minor", d.Bump)
		}
		if !d.Release {
			t.Fatal("Release = false, want true")
		}
		if got, want := d.Next.String(), "v0.4.0"; got != want {
			t.Errorf("Next = %s, want %s", got, want)
		}
	})

	// The docs-only promotion, end to end: no release, and the caller is told so
	// rather than being handed a version equal to the current one.
	t.Run("a docs-only promotion releases nothing", func(t *testing.T) {
		d, err := NextFromCommits("v0.3.4", []string{
			"docs: rewrite the README install section",
			"chore(ci): bump an action pin",
		})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if d.Release {
			t.Errorf("Release = true for a docs-only range; want false (skip quietly)")
		}
		if d.Bump != BumpNone {
			t.Errorf("bump = %v, want none", d.Bump)
		}
	})

	t.Run("an empty promotion releases nothing", func(t *testing.T) {
		d, err := NextFromCommits("v0.3.4", nil)
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if d.Release {
			t.Error("Release = true for an empty range; want false")
		}
	})

	// Bump reports what the COMMITS asked for, even where the pre-1.0 clause
	// held the version down - so the workflow log can say "major, held to minor".
	t.Run("reports the asked-for bump even when 0.x holds it down", func(t *testing.T) {
		d, err := NextFromCommits("v0.3.4", []string{"feat(config)!: rename a key"})
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if d.Bump != BumpMajor {
			t.Errorf("bump = %v, want major (what the commit asked for)", d.Bump)
		}
		if got, want := d.Next.String(), "v0.4.0"; got != want {
			t.Errorf("Next = %s, want %s (0.x holds a major to a minor)", got, want)
		}
	})

	t.Run("an unparseable current tag is an error, not a guess", func(t *testing.T) {
		if _, err := NextFromCommits("v0.4.0-rc1", []string{"feat: a"}); err == nil {
			t.Error("accepted a prerelease tag as a base; want an error")
		}
	})
}

// The real history this was designed against: the CLI merges every task PR as a
// merge commit, so a derivation that read first-parent subjects would see only
// `Merge pull request #NN` lines. Those are unrecognised, so they would bump
// PATCH forever and a `feat` would never produce a minor. This pins the
// difference so the mistake is caught in a test rather than in a wrong tag.
func TestNextFromCommits_MergeSubjectsAloneWouldMisderive(t *testing.T) {
	firstParent := []string{
		"Merge pull request #33 from lecstor/clanker/cc561415-a-session",
		"Merge pull request #32 from lecstor/clanker/f6566c70-doctor",
	}
	inner := []string{
		"feat(salvage): commit and push a dirty worktree on session end",
		"fix(doctor): report codex workdirs honestly",
	}

	fp, err := NextFromCommits("v0.3.4", firstParent)
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	in, err := NextFromCommits("v0.3.4", inner)
	if err != nil {
		t.Fatalf("errored: %v", err)
	}

	if fp.Next.String() != "v0.3.5" {
		t.Errorf("first-parent subjects derived %s, want v0.3.5 (they are unrecognised)", fp.Next)
	}
	if in.Next.String() != "v0.4.0" {
		t.Errorf("inner commits derived %s, want v0.4.0 (the feat is only visible there)", in.Next)
	}
	if fp.Next == in.Next {
		t.Fatal("first-parent and non-merge derivation agreed; the --no-merges requirement is untested")
	}
}
