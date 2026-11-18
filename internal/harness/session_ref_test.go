package harness

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

// Studio chat's session handle (studio-chat.md §3): the adapter must create a
// session under the CLI's id on a prime, continue it on a later turn, and report
// the one failure the planner must recover from — a missing session on resume.

func TestClaudeArgsCreateUnderTheSessionID(t *testing.T) {
	args := claudeArgs(Invocation{SessionRef: "11111111-2222-3333-4444-555555555555"})
	if !slices.Contains(args, "--session-id") {
		t.Fatalf("args %q do not create a session under an id", args)
	}
	if slices.Contains(args, "--resume") {
		t.Fatalf("a fresh session emitted --resume: %q", args)
	}
	i := slices.Index(args, "--session-id")
	if i+1 >= len(args) || args[i+1] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("--session-id does not carry the ref: %q", args)
	}
}

func TestClaudeArgsResumeTheSession(t *testing.T) {
	args := claudeArgs(Invocation{SessionRef: "abc", ResumeSession: true})
	if !slices.Contains(args, "--resume") {
		t.Fatalf("args %q do not resume", args)
	}
	if slices.Contains(args, "--session-id") {
		t.Fatalf("a resumed session emitted --session-id: %q", args)
	}
	i := slices.Index(args, "--resume")
	if i+1 >= len(args) || args[i+1] != "abc" {
		t.Errorf("--resume does not carry the ref: %q", args)
	}
}

func TestClaudeArgsEmitNoSessionFlagWithoutARef(t *testing.T) {
	args := claudeArgs(Invocation{})
	if slices.Contains(args, "--session-id") || slices.Contains(args, "--resume") {
		t.Errorf("a task-drain session grew a session flag: %q", args)
	}
}

func TestClaudeInvokeReportsAMissingResumedSession(t *testing.T) {
	claudeStub(t, "#!/bin/sh\necho 'No conversation found with session ID: abc' >&2\nexit 1\n")
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:        "work",
		SessionRef:    "abc",
		ResumeSession: true,
		Console:       io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.SessionMissing {
		t.Error("SessionMissing = false for claude's refused resume; the planner would keep retrying a dead handle")
	}
	if res.SessionRef != "abc" {
		t.Errorf("SessionRef = %q, want the handle the session ran as", res.SessionRef)
	}
}

func TestClaudeInvokeDoesNotCallAFreshFailureAMissingSession(t *testing.T) {
	claudeStub(t, "#!/bin/sh\necho 'No conversation found with session ID: abc' >&2\nexit 1\n")
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:     "work",
		SessionRef: "abc",
		Console:    io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.SessionMissing {
		t.Error("SessionMissing = true for a CREATE, which has no session to be missing")
	}
}

func TestOpencodeArgsResumeWithTheSessionFlag(t *testing.T) {
	args := opencodeArgs(Invocation{Prompt: "work", SessionRef: "ses_123"})
	i := slices.Index(args, "-s")
	if i < 0 || i+1 >= len(args) || args[i+1] != "ses_123" {
		t.Fatalf("args %q do not carry `-s ses_123`", args)
	}
	if got := args[len(args)-1]; got != "work" {
		t.Errorf("prompt = %q, want it behind the -- terminator", got)
	}
	if args[len(args)-2] != "--" {
		t.Errorf("args %q lost the prompt terminator", args)
	}
}

func TestOpencodeArgsEmitNoSessionFlagWithoutARef(t *testing.T) {
	if args := opencodeArgs(Invocation{Prompt: "work"}); slices.Contains(args, "-s") {
		t.Errorf("a task-drain session grew a session flag: %q", args)
	}
}

func TestResumeCapabilityMatchesTheAdapters(t *testing.T) {
	for name, want := range map[string]bool{
		"claude":   true,
		"opencode": true,
		"codex":    false,
		"copilot":  false,
	} {
		caps, ok := CapabilitiesOf(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if caps.ResumesSessions != want {
			t.Errorf("%s.ResumesSessions = %v, want %v", name, caps.ResumesSessions, want)
		}
	}
}

func TestOpencodeResultCarriesTheSessionItsStreamNamed(t *testing.T) {
	res := opencodeParsed(`{"type":"text","sessionID":"ses_live","part":{"type":"text","text":"hi"}}`)
	if res.SessionRef != "ses_live" {
		t.Errorf("SessionRef = %q, want the stream's session id", res.SessionRef)
	}
	if !strings.Contains(strings.Join(opencodeArgs(Invocation{Prompt: "x", SessionRef: "s"}), " "), "-s s") {
		// Guard the helper the assertion above relies on, so a shape change
		// cannot leave the args test passing vacuously.
		t.Error("opencodeArgs did not emit the expected `-s s`")
	}
}
