package config

import "time"

// IdleBounds is one harness's no-output watchdog: MaxIdle kills a session silent that long,
// MaxToolCall caps one open tool call while the watchdog is armed.
type IdleBounds struct {
	MaxIdle     *Duration `json:"max_idle,omitempty"`
	MaxToolCall *Duration `json:"max_tool_call,omitempty"`
}

// The shipped watchdogs.
const (
	defaultOpencodeMaxIdle = 15 * time.Minute
	defaultOpencodeMaxTool = 30 * time.Minute
	defaultClaudeMaxIdle   = 10 * time.Minute
	defaultClaudeMaxTool   = 30 * time.Minute
)

// defaultIdleBounds is the watchdog for one harness. A harness that is absent here has none,
// deliberately: its adapter does not enforce these fields, so an armed bound would only look like
// protection.
func defaultIdleBounds(harnessName string) (idle, toolCall time.Duration) {
	switch harnessName {
	case "opencode":
		return defaultOpencodeMaxIdle, defaultOpencodeMaxTool
	case "claude":
		return defaultClaudeMaxIdle, defaultClaudeMaxTool
	}
	return 0, 0
}

// SessionIdleFor resolves one harness's no-output watchdog: a field the block leaves nil inherits the
// shipped default, while an explicit 0 stays 0 and disables that bound.
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
