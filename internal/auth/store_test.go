package auth_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/auth"
)

const testToken = "clk_store_test_token_do_not_print"

// isolate points the store at a fresh file and clears the env key, so every test
// starts logged out.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clankerbar", "auth.json")
	t.Setenv(auth.EnvAuthFile, path)
	t.Setenv(auth.EnvAPIKey, "")
	return path
}

func mustSave(t *testing.T, origin string, cred auth.Credential) {
	t.Helper()
	if err := auth.Save(origin, cred); err != nil {
		t.Fatalf("Save(%s): %v", origin, err)
	}
}

func TestPathDefaultsToXDGStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv(auth.EnvAuthFile, "")
	t.Setenv("XDG_STATE_HOME", state)

	path, enabled, err := auth.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if !enabled {
		t.Fatal("Path reported the default store disabled")
	}
	if want := filepath.Join(state, "clankerbar", "auth.json"); path != want {
		t.Errorf("Path = %q, want %q", path, want)
	}
}

func TestPathFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(auth.EnvAuthFile, "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)

	path, _, err := auth.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(home, ".local", "state", "clankerbar", "auth.json"); path != want {
		t.Errorf("Path = %q, want %q", path, want)
	}
}

func TestPathDisableValues(t *testing.T) {
	for _, v := range []string{"none", "NONE", "None", os.DevNull} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(auth.EnvAuthFile, v)
			path, enabled, err := auth.Path()
			if err != nil {
				t.Fatalf("Path: %v", err)
			}
			if enabled || path != "" {
				t.Errorf("Path(%q) = (%q, %v), want the store disabled", v, path, enabled)
			}
			// A disabled store must not touch disk: a resolve answers "none".
			t.Setenv(auth.EnvAPIKey, "")
			res, err := auth.Resolve("https://clankerbar.com")
			if err != nil {
				t.Fatalf("Resolve with the store disabled: %v", err)
			}
			if res.Token != "" || res.Source != "" {
				t.Errorf("Resolve with the store disabled = %+v, want empty", res)
			}
		})
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := isolate(t)
	stateDir := filepath.Dir(path)
	mustSave(t, "https://clankerbar.com", auth.Credential{
		Token:       testToken,
		Scope:       auth.ScopeUser,
		ProjectSlug: "",
		SavedAt:     time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC),
	})

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat the store: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the store is a symlink")
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("store mode = %04o, want 0600", perm)
	}
	dfi, err := os.Lstat(stateDir)
	if err != nil {
		t.Fatalf("stat the store dir: %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("store dir mode = %04o, want 0700", perm)
	}

	st, err := auth.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Version != auth.FileVersion {
		t.Errorf("version = %d, want %d", st.Version, auth.FileVersion)
	}
	cred, ok := st.Hosts["https://clankerbar.com"]
	if !ok {
		t.Fatalf("hosts = %v, want the saved origin", st.Hosts)
	}
	if cred.Token != testToken || cred.Scope != auth.ScopeUser {
		t.Errorf("stored credential = %s, want the saved user credential", cred)
	}

	// The on-disk document is the documented schema, not an accident of the
	// struct: version 1 and a hosts map keyed by canonical origin.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int                        `json:"version"`
		Hosts   map[string]json.RawMessage `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode auth.json: %v\n%s", err, raw)
	}
	if doc.Version != 1 {
		t.Errorf("auth.json version = %d, want 1", doc.Version)
	}
	if _, ok := doc.Hosts["https://clankerbar.com"]; !ok {
		t.Errorf("auth.json hosts = %v", doc.Hosts)
	}
}

func TestSaveRefusesSymlinkedFile(t *testing.T) {
	path := isolate(t)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"hosts":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := auth.Save("https://clankerbar.com", auth.Credential{Token: testToken, Scope: auth.ScopeUser})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Save through a symlink = %v, want a symlink refusal", err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("the symlink's target was removed: %v", err)
	}
}

func TestLoadRefusesSymlinkedFile(t *testing.T) {
	path := isolate(t)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"hosts":{"https://clankerbar.com":{"token":"`+testToken+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := auth.Load()
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Load through a symlink = %v, want a symlink refusal", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the refusal leaked the token: %v", err)
	}
}

func TestSaveRefusesSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(link, "auth.json")
	t.Setenv(auth.EnvAuthFile, path)
	t.Setenv(auth.EnvAPIKey, "")

	err := auth.Save("https://clankerbar.com", auth.Credential{Token: testToken, Scope: auth.ScopeUser})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Save under a symlinked directory = %v, want a symlink refusal", err)
	}
}

func TestLoadRefusesGroupReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode check is meaningful on unix only")
	}
	path := isolate(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"hosts":{"https://clankerbar.com":{"token":"` + testToken + `","scope":"user"}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := auth.Load()
	if err == nil {
		t.Fatal("Load accepted a group-readable credential file")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the refusal does not give the fix: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the refusal leaked the token: %v", err)
	}
}

func TestLoadRefusesUnknownVersion(t *testing.T) {
	path := isolate(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"hosts":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Load(); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("Load of a future version = %v, want a version refusal", err)
	}
}

func TestDisabledStoreIgnoresAnExistingFile(t *testing.T) {
	path := isolate(t)
	mustSave(t, "https://clankerbar.com", auth.Credential{Token: testToken, Scope: auth.ScopeUser})

	t.Setenv(auth.EnvAuthFile, "none")
	res, err := auth.Resolve("https://clankerbar.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != "" {
		t.Errorf("a disabled store still returned a credential: %s", res)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("disabling the store touched the file: %v", err)
	}
}

func TestSaveReplacesTheSameOrigin(t *testing.T) {
	isolate(t)
	mustSave(t, "https://clankerbar.com", auth.Credential{Token: "first", Scope: auth.ScopeUser})
	mustSave(t, "https://clankerbar.com", auth.Credential{Token: "second", Scope: auth.ScopeProject, ProjectSlug: "acme"})

	st, err := auth.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Hosts) != 1 {
		t.Errorf("hosts = %v, want one entry after a replace", st.Hosts)
	}
	res, err := auth.Resolve("https://clankerbar.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != "second" || res.Scope != auth.ScopeProject {
		t.Errorf("Resolve = %s, want the replacement credential", res)
	}
}

func TestResolveEnvWinsOverStore(t *testing.T) {
	isolate(t)
	mustSave(t, "https://clankerbar.com", auth.Credential{Token: testToken, Scope: auth.ScopeUser})
	t.Setenv(auth.EnvAPIKey, "env-key")

	res, err := auth.Resolve("https://clankerbar.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != "env-key" || res.Source != auth.SourceEnv {
		t.Errorf("Resolve = %+v, want the env key", res)
	}
}

func TestResolveStoredCredential(t *testing.T) {
	path := isolate(t)
	mustSave(t, "https://clankerbar.com", auth.Credential{
		Token:       testToken,
		Scope:       auth.ScopeProject,
		ProjectSlug: "acme",
	})

	res, err := auth.Resolve("https://clankerbar.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != testToken || res.Source != auth.SourceFile {
		t.Errorf("Resolve = %+v, want the stored credential", res)
	}
	if res.Path != path {
		t.Errorf("Resolution.Path = %q, want %q", res.Path, path)
	}
	if res.Scope != auth.ScopeProject || res.ProjectSlug != "acme" {
		t.Errorf("Resolution scope = %q/%q, want project/acme", res.Scope, res.ProjectSlug)
	}
}

func TestResolveNeverCrossesOrigins(t *testing.T) {
	isolate(t)
	mustSave(t, "https://one.example", auth.Credential{Token: testToken, Scope: auth.ScopeUser})

	res, err := auth.Resolve("https://two.example")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != "" {
		t.Errorf("Resolve returned origin one's token for origin two: %s", res)
	}

	// A canonical spelling of the same origin DOES match, including a
	// capitalised host (host names are case-insensitive).
	mustSave(t, "https://Case.Example", auth.Credential{Token: "case-token", Scope: auth.ScopeUser})
	res, err = auth.Resolve("https://case.example")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != "case-token" {
		t.Errorf("case-insensitive lookup missed: %s", res)
	}
}

func TestResolveRefusesAnUnsafeOrigin(t *testing.T) {
	isolate(t)
	if _, err := auth.Resolve("http://attacker.example"); err == nil {
		t.Fatal("Resolve accepted plain http to a non-loopback host")
	}
}

func TestSaveRefusesAnUnsafeOrigin(t *testing.T) {
	isolate(t)
	err := auth.Save("http://attacker.example", auth.Credential{Token: testToken, Scope: auth.ScopeUser})
	if err == nil {
		t.Fatal("Save accepted plain http to a non-loopback host")
	}
}

func TestRemoveAndRemoveAll(t *testing.T) {
	path := isolate(t)
	mustSave(t, "https://one.example", auth.Credential{Token: "one", Scope: auth.ScopeUser})
	mustSave(t, "https://two.example", auth.Credential{Token: "two", Scope: auth.ScopeUser})

	removed, err := auth.Remove("https://one.example")
	if err != nil || !removed {
		t.Fatalf("Remove = (%v, %v), want (true, nil)", removed, err)
	}
	if _, _, ok, err := auth.Lookup("https://one.example"); err != nil || ok {
		t.Fatalf("Lookup after Remove = (%v, %v), want not found", ok, err)
	}
	if _, _, ok, err := auth.Lookup("https://two.example"); err != nil || !ok {
		t.Fatalf("the other origin was removed: (%v, %v)", ok, err)
	}
	// Idempotent.
	if removed, err := auth.Remove("https://one.example"); err != nil || removed {
		t.Fatalf("second Remove = (%v, %v), want (false, nil)", removed, err)
	}

	n, err := auth.RemoveAll()
	if err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if n != 1 {
		t.Errorf("RemoveAll dropped %d entries, want 1", n)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("RemoveAll left the file behind: %v", err)
	}
	if n, err := auth.RemoveAll(); err != nil || n != 0 {
		t.Fatalf("RemoveAll on a missing store = (%d, %v), want (0, nil)", n, err)
	}
}

func TestDisabledStoreRefusesSaveButNotRemove(t *testing.T) {
	t.Setenv(auth.EnvAuthFile, os.DevNull)
	t.Setenv(auth.EnvAPIKey, "")

	err := auth.Save("https://clankerbar.com", auth.Credential{Token: testToken, Scope: auth.ScopeUser})
	if !errors.Is(err, auth.ErrStoreDisabled) {
		t.Fatalf("Save with the store disabled = %v, want ErrStoreDisabled", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the refusal leaked the token: %v", err)
	}
	if removed, err := auth.Remove("https://clankerbar.com"); err != nil || removed {
		t.Fatalf("Remove with the store disabled = (%v, %v), want (false, nil)", removed, err)
	}
	if n, err := auth.RemoveAll(); err != nil || n != 0 {
		t.Fatalf("RemoveAll with the store disabled = (%d, %v), want (0, nil)", n, err)
	}
}

func TestSaveRejectsEmptyTokenAndUnknownScope(t *testing.T) {
	isolate(t)
	if err := auth.Save("https://clankerbar.com", auth.Credential{}); err == nil {
		t.Fatal("Save accepted an empty token")
	}
	if err := auth.Save("https://clankerbar.com", auth.Credential{Token: testToken, Scope: "galaxy"}); err == nil {
		t.Fatal("Save accepted an unknown scope")
	}
}

// A credential must not be printable through an accidental %v of the types
// that hold it.
func TestCredentialAndStoreRedactTokens(t *testing.T) {
	c := auth.Credential{Token: testToken, Scope: auth.ScopeUser}
	if s := c.String(); strings.Contains(s, testToken) {
		t.Errorf("Credential.String leaked the token: %s", s)
	}
	st := auth.Store{Version: 1, Hosts: map[string]auth.Credential{"https://x": c}}
	if s := st.String(); strings.Contains(s, testToken) {
		t.Errorf("Store.String leaked the token: %s", s)
	}
	res := auth.Resolution{Token: testToken, Source: auth.SourceFile}
	if s := res.String(); strings.Contains(s, testToken) {
		t.Errorf("Resolution.String leaked the token: %s", s)
	}
}

// A directory another user can write lets them replace the 0600 file, so the
// store is refused before anything is written into it.
func TestSaveRefusesGroupWritableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode check is meaningful on unix only")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.json")
	t.Setenv(auth.EnvAuthFile, path)
	t.Setenv(auth.EnvAPIKey, "")

	err := auth.Save("https://clankerbar.com", auth.Credential{Token: testToken, Scope: auth.ScopeUser})
	if err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("Save under a group-writable directory = %v, want a mode refusal", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the refusal leaked the token: %v", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("Save wrote the credential file despite the refusal: %v", statErr)
	}
	if _, loadErr := auth.Load(); loadErr == nil {
		t.Error("Load accepted a group-writable store directory")
	}
}

// A file another user owns can be rewritten by them whatever its mode says, so
// it is refused. Assigning a foreign owner needs root.
func TestLoadRefusesForeignOwnedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the ownership check is meaningful on unix only")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root to assign a foreign owner")
	}
	path := isolate(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"hosts":{"https://clankerbar.com":{"token":"` + testToken + `","scope":"user"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 65534, 65534); err != nil {
		t.Skipf("cannot assign a foreign owner: %v", err)
	}

	_, err := auth.Load()
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("Load of a foreign-owned file = %v, want an owner refusal", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the refusal leaked the token: %v", err)
	}
}

// Logging out of the last origin deletes the store rather than leaving an empty
// document behind.
func TestRemoveDropsTheFileWithTheLastOrigin(t *testing.T) {
	path := isolate(t)
	mustSave(t, "https://one.example", auth.Credential{Token: "one", Scope: auth.ScopeUser})

	removed, err := auth.Remove("https://one.example")
	if err != nil || !removed {
		t.Fatalf("Remove = (%v, %v), want (true, nil)", removed, err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("Remove left the last origin's file behind: %v", statErr)
	}
}

// Remove and RemoveAll refuse a symlink or a directory at the store path: either
// would make "forget my key" mean something other than the file it names.
func TestRemovePathsRefuseSymlinkAndDirectoryStores(t *testing.T) {
	for _, removeAll := range []bool{false, true} {
		name := "Remove"
		if removeAll {
			name = "RemoveAll"
		}
		t.Run(name+"/symlink", func(t *testing.T) {
			path := isolate(t)
			target := filepath.Join(t.TempDir(), "elsewhere.json")
			if err := os.WriteFile(target, []byte(`{"version":1,"hosts":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			err := removeOne(t, removeAll)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("%s through a symlink = %v, want a symlink refusal", name, err)
			}
			if _, statErr := os.Lstat(target); statErr != nil {
				t.Errorf("the symlink's target was removed: %v", statErr)
			}
		})
		t.Run(name+"/directory", func(t *testing.T) {
			path := isolate(t)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}

			err := removeOne(t, removeAll)
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("%s of a directory = %v, want a regular-file refusal", name, err)
			}
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Errorf("the directory was removed: %v", statErr)
			}
		})
	}
}

func removeOne(t *testing.T, removeAll bool) error {
	t.Helper()
	if removeAll {
		_, err := auth.RemoveAll()
		return err
	}
	_, err := auth.Remove("https://clankerbar.com")
	return err
}
