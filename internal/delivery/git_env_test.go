package delivery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRunOutRedactsURLCredentialsFromStderr(t *testing.T) {
	const secret = "gho_super_secret_token_966"
	dir := t.TempDir()
	fakeGit := filepath.Join(t.TempDir(), "git")
	writeFile(t, fakeGit, `#!/bin/sh
echo "fatal: unable to access 'https://x-access-token:`+secret+`@github.com/lecstor/private.git/': Could not resolve host: github.com" >&2
exit 128
`)
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatal(err)
	}

	v := New(dir, "origin")
	v.gitBin = fakeGit
	_, err := v.gitRunOut(context.Background(), dir, ScopeEnv(secret), "fetch", "origin")
	if err == nil {
		t.Fatal("expected error from failing git command")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "x-access-token:") {
		t.Fatalf("gitRunOut leaked credential in error: %q", msg)
	}
	if !strings.Contains(msg, "https://redacted@github.com/lecstor/private.git/") {
		t.Fatalf("gitRunOut should preserve redacted URL for diagnosis, got %q", msg)
	}
}

func TestGitRunOutStripsInheritedGitConfigAndOwnedVars(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "stale-user")
	t.Setenv("GIT_CONFIG_KEY_1", "user.email")
	t.Setenv("GIT_CONFIG_VALUE_1", "stale@example.com")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_ASKPASS", "/bin/evil")
	t.Setenv("SSH_ASKPASS", "/bin/evil")
	t.Setenv("GIT_SSH_COMMAND", "ssh -oBatchMode=no")
	t.Setenv("GCM_INTERACTIVE", "always")

	dir := t.TempDir()
	fakeGit := filepath.Join(t.TempDir(), "git")
	writeFile(t, fakeGit, "#!/bin/sh\nenv\n")
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatal(err)
	}

	v := New(dir, "origin")
	v.gitBin = fakeGit

	out, err := v.gitRunOut(context.Background(), dir, ScopeEnv(credFakeToken), "fetch", "origin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	counts := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		k := envName(line)
		counts[k] = append(counts[k], line)
	}
	for k, want := range map[string]string{
		"GIT_CONFIG_COUNT":    "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0":    "GIT_CONFIG_KEY_0=" + wantKey0,
		"GIT_CONFIG_VALUE_0":  "GIT_CONFIG_VALUE_0=" + wantVal0,
		"GIT_TERMINAL_PROMPT": "GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS":         "GIT_ASKPASS=echo",
		"SSH_ASKPASS":         "SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND":     "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE":     "GCM_INTERACTIVE=never",
	} {
		if got := counts[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want single [%q]", k, got, want)
		}
	}
	for _, absent := range []string{"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		if got := counts[absent]; len(got) != 0 {
			t.Errorf("inherited %s should be stripped, got %v", absent, got)
		}
	}

	// Unscoped call must carry no inherited GIT_CONFIG_* at all.
	unscoped, err := v.gitRunOut(context.Background(), dir, nil, "status")
	if err != nil {
		t.Fatalf("unscoped unexpected error: %v", err)
	}
	for _, line := range strings.Split(unscoped, "\n") {
		if isGitConfigEnv(envName(line)) {
			t.Errorf("unscoped gitRunOut leaked inherited git config env: %q", line)
		}
	}
}
