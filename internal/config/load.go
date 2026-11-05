package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func defaults() *Config {
	return &Config{
		Harness:    "claude",
		Prompt:     defaultPrompt,
		BacklogURL: defaultBacklogURL,
	}
}

// cwdConfigName is the file that used to be auto-discovered from the process
// working directory, ahead of the operator's own config. It no longer is - see
// refuseImplicitWorkDirConfig - but the name is still recognised so its presence
// can be refused loudly rather than ignored silently.
const cwdConfigName = "clankerbar.json"

// homeConfigRelPath is the one config file this tool discovers on its own,
// relative to the user's home directory.
var homeConfigRelPath = filepath.Join(".config", "clankerbar", "config.json")

// Load reads config from path (or a discovered default), layered over defaults.
// An explicit path that cannot be read is an error; a missing default file is not.
//
// DISCOVERY IS HOME-ONLY (CLA-260). An explicit --config is honoured wherever it
// points, including into the working directory; what is gone is the implicit
// candidate. See refuseImplicitWorkDirConfig.
func Load(path string) (*Config, error) {
	cfg := defaults()
	p := path
	if p == "" {
		if err := refuseImplicitWorkDirConfig(); err != nil {
			return nil, err
		}
		p = discover()
	}
	if p == "" {
		return cfg, nil
	}
	data, err := readOwnerOnly(p, groupOtherWrite)
	if err != nil {
		if path == "" && errors.Is(err, os.ErrNotExist) {
			return cfg, nil // discovered default absent — fine
		}
		if errors.Is(err, errInsecureMode) {
			return nil, fmt.Errorf("%w: anyone who can write it owns the prompt, the permission policy and the child environment of the next unattended run - chmod go-w %s", err, p)
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	// CLA-639: whether the file named `review_panel` with a JSON ARRAY. A
	// present-but-empty array is the operator's own panel of none, which Validate
	// refuses; absent (nil after unmarshal), or an explicit JSON `null` (what a
	// marshalled config with no panel used to write), is the single reviewer,
	// exactly as before the panel existed. Only an array counts as set: `null`
	// carries no panel to honour.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err == nil {
		if raw, ok := probe["review_panel"]; ok && strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			cfg.reviewPanelSet = true
		}
	}
	cfg.source = p
	return cfg, nil
}

func discover() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, homeConfigRelPath)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// refuseImplicitWorkDirConfig refuses to run when a `clankerbar.json` is sitting
// in the process working directory and no --config was given.
//
// It used to be the FIRST discovery candidate, stat'd as a relative path against
// whatever cwd the process happened to have, ahead of the operator's own
// ~/.config file, with no ownership or provenance check. Everything in it is
// load-bearing for an unattended run: `prompt` is the entire instruction the
// fresh session gets, `settings_path` is the headless allow/deny policy,
// `env` is arbitrary environment for the child (including secrets read off
// disk), and `backlog_url` is the one origin the account-scoped API key may be
// sent to (CLA-257 - a fix that rests on this file being the operator's).
//
// The working directory is exactly where that trust does not hold. It is a
// checkout, which may have been cloned from anywhere, and the sessions this
// daemon spawns run there WITH EDIT PERMISSION - so a session can write the file
// that owns the NEXT run's prompt, policy and environment. Config is read once at
// startup, so the damage lands on tomorrow's cron, not on the run that wrote it.
//
// REFUSED, NOT IGNORED. Ignoring it would silently fall back to the home config
// or to bare defaults, which for an operator who genuinely relied on cwd
// discovery means an unattended loop running a different prompt against a
// different backlog and saying nothing - the same class of silent-wrong-config
// this closes. A refusal costs the honest operator one flag they are already
// shown (`-c ./clankerbar.json`), and turns the hostile case into a stopped
// daemon a human looks at rather than a captured one nobody does.
func refuseImplicitWorkDirConfig() error {
	fi, err := os.Stat(cwdConfigName)
	if err != nil || fi.IsDir() {
		return nil
	}
	shown := cwdConfigName
	if abs, err := filepath.Abs(cwdConfigName); err == nil {
		shown = abs
	}
	return fmt.Errorf(
		"refusing to auto-load %s: a config file in the working directory is no longer discovered implicitly - it decides the prompt, the permission policy, the child environment and the API key's destination for every session this loop spawns, and the working directory is a checkout the sessions themselves can write. Name it if it is yours (--config %s), or move it to ~/%s",
		shown, cwdConfigName, homeConfigRelPath,
	)
}

// Overrides carries explicit flag values; zero values are left untouched.
type Overrides struct {
	Harness          string
	Model            string
	WorkDir          string
	ConfigDir        string
	MaxIterations    int
	PollInterval     time.Duration
	IdlePollInterval time.Duration
}

// ApplyFlagOverrides layers non-zero flag values over the loaded config.
func (c *Config) ApplyFlagOverrides(o Overrides) {
	if o.Harness != "" {
		c.Harness = o.Harness
		c.harnessFromFlag = true
	}
	if o.Model != "" {
		c.Model = o.Model
		c.modelFromFlag = true
	}
	if o.WorkDir != "" {
		c.WorkDir = o.WorkDir
	}
	if o.ConfigDir != "" {
		c.ConfigDir = o.ConfigDir
	}
	if o.MaxIterations != 0 {
		c.MaxIterations = o.MaxIterations
	}
	if o.PollInterval != 0 {
		c.PollInterval = Duration(o.PollInterval)
	}
	if o.IdlePollInterval != 0 {
		c.IdlePollInterval = Duration(o.IdlePollInterval)
	}
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

// Permission bits that disqualify a file this loop is about to TRUST.
//
//   - groupOtherWrite: someone other than the owner can REWRITE it. Fatal for a
//     config file, whose contents choose what an unattended session is told to do
//     and what it is allowed to call.
//   - groupOtherAccess: someone other than the owner can READ it (write implied).
//     Fatal for a secret - the whole point of the `@path` indirection.
const (
	groupOtherWrite  os.FileMode = 0o022
	groupOtherAccess os.FileMode = 0o077
)

// errInsecureMode is the sentinel every mode refusal wraps, so callers can add
// their own "and here is why that matters" without re-deriving the mode.
var errInsecureMode = errors.New("insecure file mode")

// readOwnerOnly reads a file the loop is about to trust, refusing it when anyone
// but the owner (or root) could have decided its contents.
//
// The file's own mode is taken from the OPEN FILE HANDLE, not from a separate
// os.Stat of the path, so there is no window between the mode that was checked
// and the bytes that were read: a file swapped after the check is a different
// inode, and this reads the one it vetted. Symlinks are followed (the target's
// mode is what matters), which is deliberate - an operator pointing at
// ~/.secrets/token through a symlink is normal.
func readOwnerOnly(path string, forbid os.FileMode) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := vetTrustedFile(path, fi, forbid); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// vetTrustedFile is the three questions a mode check has to answer to mean what
// it says. Only the first is about the file's own bits:
//
//  1. Can group or other CHANGE (or, for a secret, READ) it?
//  2. Is it OWNED by someone else? A 0600 file is unreadable by a peer, so under a
//     normal uid this is nearly self-enforcing - but a root daemon reading a
//     user-owned config is exactly the case where mode alone says "fine" and the
//     file is under someone else's control.
//  3. Can group or other REPLACE it by writing its directory? A 0600 token in a
//     0777 directory can be unlinked and recreated by any local account, which
//     defeats (1) entirely. This checks the immediate parent only, not the whole
//     ancestor chain - a world-writable /home is its own, larger problem, and
//     OpenSSH's StrictModes draws the line in the same place. The sticky bit is
//     the documented exception (/tmp), where only an entry's owner may remove it.
func vetTrustedFile(path string, fi os.FileInfo, forbid os.FileMode) error {
	if !permissionBitsAreMeaningful {
		// Go synthesises a fixed mode for ordinary files on such platforms, so the
		// bits carry no information and enforcing them would refuse every config
		// the tool has - a denial, not a defence.
		return nil
	}
	if perm := fi.Mode().Perm(); perm&forbid != 0 {
		verb := "writable by group or other"
		if forbid&groupOtherAccess == groupOtherAccess {
			verb = "readable by group or other"
		}
		return fmt.Errorf("%w: %s is %s (mode %04o)", errInsecureMode, path, verb, perm)
	}
	if uid, ok := fileOwnerUID(fi); ok {
		if me := os.Geteuid(); uid != me && uid != 0 {
			return fmt.Errorf("%w: %s is owned by uid %d, not by you (uid %d) or root - its owner decides its contents whatever its mode says", errInsecureMode, path, uid, me)
		}
	}
	dir := filepath.Dir(path)
	if dfi, err := os.Stat(dir); err == nil {
		if m := dfi.Mode(); m.Perm()&groupOtherWrite != 0 && m&os.ModeSticky == 0 {
			return fmt.Errorf("%w: %s sits in %s, which is writable by group or other (mode %04o) - anyone who can write the directory can replace the file whatever the file's own mode is", errInsecureMode, path, dir, m.Perm())
		}
	}
	return nil
}

// refuseInsecureMode vets a file the loop hands ONWARD rather than reads, so the
// same rule covers a path whose contents are another program's business.
//
// Only an insecure-mode verdict is returned: absence and unreadability belong to
// whichever check already reports them (for settings_path, `doctor`'s permissions
// check, which says what a missing policy file means far better than a config
// error could).
func refuseInsecureMode(path string, forbid os.FileMode) error {
	if path == "" {
		return nil
	}
	if _, err := readOwnerOnly(path, forbid); errors.Is(err, errInsecureMode) {
		return err
	}
	return nil
}

// underWorkDir resolves a RELATIVE path the way the spawned harness will: against
// the session's working directory, not against the daemon's.
//
// The two are routinely different, and every one of these paths is handed to the
// child verbatim while `cmd.Dir` is the workdir (harness/claude.go), so a
// relative value used to be read by US against one directory and by the CHILD
// against another. That is not a cosmetic mismatch: `mcp_config_path: ".mcp.json"`
// with a workdir elsewhere made checkMCPConfigOrigins vet a file that did not
// exist (absent is the one benign case, so the gate passed) while the session
// loaded the checkout's file with its own origins and its own `Authorization`
// headers - the CLA-257 property defeated by a relative path, with `doctor` green
// throughout. Resolving here means the file that is VETTED is provably the file
// that is USED.
//
// An empty workdir means the child inherits our cwd, so relative already means
// the same thing to both and is left alone.
func underWorkDir(p, workdir string) string {
	if p == "" || workdir == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workdir, p)
}

// RedactURLCredentials removes any URL userinfo - the `user:password@` part of
// `scheme://user:password@host/...` - from raw, so a clone URL base carrying
// embedded mirror credentials can be logged or quoted in an error without
// writing the credential down. The host and path are kept: they are what makes
// a log line or a failure diagnosable.
//
// An scp-style ssh prefix (`git@github-personal:`) is not a URL and its user is
// a plain login name, not a credential, so it is returned unchanged. A value
// with no userinfo is returned byte for byte.
func RedactURLCredentials(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	authority := rest
	if end := strings.IndexByte(rest, '/'); end >= 0 {
		authority = rest[:end]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return raw
	}
	return raw[:i+3] + "redacted@" + rest[at+1:]
}

// ValidateCloneURLBase checks that base can be concatenated with an
// `owner/name.git` identity to make ONE clone URL. The value is a prefix, so
// the only structural mistake that is both common and silent is a missing
// separator - `https://github.com` would otherwise produce
// `https://github.comowner/name.git` - and that is refused here.
//
// Accepted shapes:
//
//	https://github.com/        (any URL-style base, ending in /)
//	file:///srv/mirrors/       (a local mirror)
//	/srv/mirrors/              (a bare path prefix)
//	git@github-personal:       (an scp-style ssh host alias)
//
// An empty value is the default and therefore valid. Errors are recorded
// with the key name by the caller, so they describe only what is wrong with
// the value.
func ValidateCloneURLBase(base string) error {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	if strings.ContainsAny(base, " \t\r\n") {
		return errors.New("must not contain whitespace")
	}
	if strings.Contains(base, "://") {
		if !strings.HasSuffix(base, "/") {
			return fmt.Errorf("%q must end with / - it is a URL prefix (https://github.com/)", RedactURLCredentials(base))
		}
		return nil
	}
	if strings.HasSuffix(base, ":") {
		host := strings.TrimSuffix(base, ":")
		at := strings.LastIndexByte(host, '@')
		// HasSuffix(host, ":") is the doubled separator - `git@host::` leaves a
		// host that still ends in ':' after one is trimmed, and the concatenated
		// URL is uncloneable (git reads the second ':' as the start of a path).
		if at <= 0 || at == len(host)-1 || strings.HasSuffix(host, ":") || strings.Contains(host, "/") {
			return fmt.Errorf("%q is not an ssh host alias - write it as user@host: (git@github-personal:)", RedactURLCredentials(base))
		}
		return nil
	}
	if strings.HasSuffix(base, "/") {
		return nil
	}
	return fmt.Errorf("%q must end with / (https://github.com/, file:///srv/mirrors/) or be an ssh host alias ending in : (git@github-personal:)", RedactURLCredentials(base))
}
