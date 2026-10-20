package config

// CLA-713: the clone transport the supervisor derives a fleet clone's URL
// from. The key must default to the historical https://github.com/ byte for
// byte, accept an ssh host-alias prefix, and refuse a value that would be
// concatenated into a URL nobody can clone.

import (
	"encoding/json"
	"strings"
	"testing"
)

// The default is the behaviour every config that never sets the key keeps.
func TestDefaultCloneURLBaseIsTheHistoricalHTTPSBase(t *testing.T) {
	if DefaultCloneURLBase != "https://github.com/" {
		t.Fatalf("DefaultCloneURLBase = %q, want https://github.com/ - an unset key must not change any clone's transport", DefaultCloneURLBase)
	}
}

func TestEffectiveCloneURLBase(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want string
	}{
		{"unset keeps the default", "", DefaultCloneURLBase},
		{"an ssh host alias is used verbatim", "git@github-personal:", "git@github-personal:"},
		{"a mirror prefix is used verbatim", "https://git.example.com/mirror/", "https://git.example.com/mirror/"},
		{"surrounding whitespace is trimmed", "  git@github-personal: \n", "git@github-personal:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{CloneURLBase: c.set}
			if got := cfg.EffectiveCloneURLBase(); got != c.want {
				t.Errorf("EffectiveCloneURLBase() = %q, want %q", got, c.want)
			}
		})
	}
}

// The accepted shapes, and the common mistakes refused with a message that
// names what a prefix has to end with.
func TestValidateCloneURLBase(t *testing.T) {
	valid := []string{
		"",
		DefaultCloneURLBase,
		"https://github.com/",
		"http://mirror.internal/git/",
		"file:///srv/mirrors/",
		"/srv/mirrors/",
		"git@github-personal:",
		"git@github.com:",
		"deploy@git.internal:",
		"  git@github-personal:  ", // surrounding whitespace is trimmed
	}
	for _, base := range valid {
		if err := ValidateCloneURLBase(base); err != nil {
			t.Errorf("ValidateCloneURLBase(%q) = %v, want nil", base, err)
		}
	}

	invalid := []struct {
		base string
		want string
	}{
		{"https://github.com", "must end with /"},
		{"git@github-personal", "must end with /"},
		{"/srv/mirrors", "must end with /"},
		{"https://github.com/ has a space/", "whitespace"},
		{"git@:", "ssh host alias"},
		{"@github-personal:", "ssh host alias"},
		{"https://github.com/:", "must end with /"},
	}
	for _, c := range invalid {
		err := ValidateCloneURLBase(c.base)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateCloneURLBase(%q) = %v, want an error containing %q", c.base, err, c.want)
		}
	}
}

// Config.Validate is where doctor and run see the key, so a malformed prefix
// is refused there rather than reaching the supervisor's clone.
func TestValidateAcceptsAndRefusesCloneURLBase(t *testing.T) {
	cfg := func(base string) *Config {
		return &Config{Harness: "claude", Prompt: "Work.", CloneURLBase: base}
	}
	if err := cfg("git@github-personal:").Validate(); err != nil {
		t.Errorf("an ssh clone_url_base must validate: %v", err)
	}
	padded := cfg("  https://github.com/  ")
	if err := padded.Validate(); err != nil {
		t.Errorf("a padded clone_url_base must validate: %v", err)
	}
	if padded.CloneURLBase != "https://github.com/" {
		t.Errorf("Validate left CloneURLBase as %q, want it trimmed", padded.CloneURLBase)
	}
	err := cfg("https://github.com").Validate()
	if err == nil || !strings.Contains(err.Error(), "clone_url_base") {
		t.Fatalf("Validate() = %v, want a clone_url_base refusal", err)
	}
}

// omitempty is load-bearing: Config.Identity() hashes the marshalled form, so
// an unset key must not change a daemon's beacon identity. A set one changes
// the identity, correctly - the clone transport is part of the config.
func TestCloneURLBaseAndTheConfigIdentity(t *testing.T) {
	unset := &Config{Harness: "claude", Prompt: "Work."}
	alsoUnset := &Config{Harness: "claude", Prompt: "Work."}
	if unset.Identity() != alsoUnset.Identity() {
		t.Fatal("two configs with no clone_url_base have different identities")
	}
	b, err := json.Marshal(unset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "clone_url_base") {
		t.Fatalf("an unset clone_url_base is marshalled, so every existing config's identity would change: %s", b)
	}
	set := &Config{Harness: "claude", Prompt: "Work.", CloneURLBase: "git@github-personal:"}
	if set.Identity() == unset.Identity() {
		t.Fatal("setting clone_url_base did not change the config identity")
	}
}
