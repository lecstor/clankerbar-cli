package config

import "time"

// IdleBounds is one harness's no-output watchdog: MaxIdle kills a session silent that long,
// MaxToolCall caps one open tool call while the watchdog is armed. "0" disables a bound.
type IdleBounds struct {
	MaxIdle     *Duration `json:"max_idle,omitempty"`
	MaxToolCall *Duration `json:"max_tool_call,omitempty"`
}

// The shipped watchdogs. Both harnesses that enforce the idle guard get a
// per-call cap, because each adapter can tell when a call is in flight: claude
// from its tool_use/tool_result blocks, opencode from the step whose tools it
// cannot see until they settle - step_start opens the window and step_finish
// closes it (CLA-900). The cap is what bounds a call that never returns; without
// one the paused idle clock would leave a stuck call unbounded.
//
// opencode's idle bound sits above its own 10-minute bash-tool ceiling, so a
// 10-minute call survives even when the adapter never sees the step opened. A
// LONGER call that never produces a step_start is still idle-killed: a stream
// that stopped producing events is read as silence, not as a call.
const (
	defaultOpencodeMaxIdle = 15 * time.Minute
	defaultOpencodeMaxTool = 30 * time.Minute
	defaultClaudeMaxIdle   = 10 * time.Minute
	defaultClaudeMaxTool   = 30 * time.Minute
)

// defaultIdleBounds is the watchdog for one harness. A harness that is absent
// here has none, deliberately: its adapter does not enforce these fields, so an
// armed bound would only look like protection.
func defaultIdleBounds(harnessName string) (idle, toolCall time.Duration) {
	switch harnessName {
	case "opencode":
		return defaultOpencodeMaxIdle, defaultOpencodeMaxTool
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
