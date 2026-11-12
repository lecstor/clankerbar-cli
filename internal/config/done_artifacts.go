package config

import "strings"

// dsmlTag is the full-width bar DeepSeek wraps its tool-call markup in. The model
// sometimes leaks the closing tags into its text after the report (seen live on
// 2026-11-13), and they are never part of a report.
const dsmlTag = "｜DSML｜"

// stripTrailingModelArtifacts drops trailing lines that are only leaked
// tool-call markup, so a valid report is not refused for text the session never
// meant to write. Any other trailing text still reaches the decoder's refusal.
func stripTrailingModelArtifacts(body string) string {
	lines := strings.Split(body, "\n")
	end := len(lines)
	for end > 0 {
		l := strings.TrimSpace(lines[end-1])
		if l != "" && !(strings.HasPrefix(l, "<") && strings.HasSuffix(l, ">") && strings.Contains(l, dsmlTag)) {
			break
		}
		end--
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n"))
}
