package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The re-exec must go through the LAUNCH path, not the resolved binary: an
// operator who starts the daemon through a stable symlink (~/.local/bin/
// clankerbar, or bare `clankerbar` on PATH) has to pick up a NEW build on
// restart. os.Executable() is deliberately the last resort, because on Linux it
// resolves /proc/self/exe through symlinks and would pin the daemon to the
// exact inode it was born as.
func TestLaunchBinaryPrefersTheLaunchPath(t *testing.T) {
	t.Run("explicit path reused untouched", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "clankerbar")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := launchBinary([]string{bin, "run"})
		if err != nil {
			t.Fatalf("launchBinary: %v", err)
		}
		if got != bin {
			t.Errorf("got %q, want the exact path launched %q", got, bin)
		}
	})

	t.Run("bare name resolved via PATH keeps the symlink", func(t *testing.T) {
		binDir := t.TempDir()
		stable := filepath.Join(binDir, "clankerbar") // the stable symlink's name
		if err := os.WriteFile(stable, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		got, err := launchBinary([]string{"clankerbar", "run", "--harness=claude"})
		if err != nil {
			t.Fatalf("launchBinary: %v", err)
		}
		if got != stable {
			t.Errorf("got %q, want %q from PATH - restarting must follow the launch path", got, stable)
		}
	})

	t.Run("unusable argv0 falls back to os.Executable", func(t *testing.T) {
		got, err := launchBinary([]string{"/definitely/not/anywhere/clankerbar", "run"})
		if err != nil {
			t.Fatalf("launchBinary: %v", err)
		}
		if got == "" {
			t.Error("fallback returned an empty path")
		}
		if got == "/definitely/not/anywhere/clankerbar" {
			t.Errorf("the unusable argv[0] leaked through as the exec target")
		}
	})
}

// restartSelf itself is one syscall away from replacing the TEST process, so it
// is pinned only by inspection: the unix file's body is Exec(bin, argv,
// os.Environ()) with nothing else, and its error path returns without side
// effects. Everything decidable before that call is covered above.
