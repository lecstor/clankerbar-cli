package config

import "time"

// IdleBounds is one harness's no-output watchdog: MaxIdle kills a session silent that long,
// MaxToolCall caps one open tool call while the watchdog is armed. "0" disables a bound.
type IdleBounds struct {
	MaxIdle     *Duration `json:"max_idle,omitempty"`
	MaxToolCall *Duration `json:"max_tool_call,omitempty"`
}

// The shipped watchdogs. opencode's idle bound sits above its own 10-minute
// bash-tool ceiling because opencode streams only SETTLED tool parts: a running
// tool emits nothing, so the idle clock never pauses and a short bound would
// kill honest long commands. claude's clock does pause while a call is open,
// which is what lets it take a shorter idle bound plus a per-call cap.
const (
	defaultOpencodeMaxIdle = 15 * time.Minute
	defaultClaudeMaxIdle   = 10 * time.Minute
	defaultClaudeMaxTool   = 30 * time.Minute
)

// defaultIdleBounds is the watchdog for one harness. A harness that is absent
// here has none, deliberately: its adapter does not enforce these fields, so an
// armed bound would only look like protection.
func defaultIdleBounds(harnessName string) (idle, toolCall time.Duration) {
	switch harnessName {
	case "opencode":
		return defaultOpencodeMaxIdle, 0
	case "claude":
		return defaultClaudeMaxIdle, defaultClaudeMaxTool
	}
	return 0, 0
}

// SessionIdleFor resolves one harness's no-output watchdog: the operator's own
// bounds when the block names them, else the shipped default. A bound the
// operator set to 0 stays 0 — that is how it is turned off.
func (c *Config) SessionIdleFor(harnessName string) (idle, toolCall time.Duration) {
	idle, toolCall = defaultIdleBounds(harnessName)
	b, ok := c.SessionIdle[harnessName]
	if !ok {
		return idle, toolCall
	}
	if b.MaxIdle != nil {
		idle = b.MaxIdle.Duration()
	}
	if b.MaxToolCall != nil {
		toolCall = b.MaxToolCall.Duration()
	}
	return idle, toolCall
}
