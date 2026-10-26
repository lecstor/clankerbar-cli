package backlog

import (
	"encoding/json"
	"testing"
)

// The run-config version rides the poll this payload already is (CLA-410).
// Absent from an older plane it decodes to 0 - which is ALSO the "nothing
// stored" answer - so an old plane degrades to today's behaviour with no
// version negotiation.
func TestParseSummary_RunConfigVersion(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"version":          7,
			"runConfigVersion": 3,
			"counts":           map[string]any{"ready": 1},
			"claimable":        1,
		})
		s, err := parseSummary(body)
		if err != nil {
			t.Fatalf("parseSummary: %v", err)
		}
		if s.RunConfigVersion != 3 {
			t.Errorf("RunConfigVersion = %d, want 3", s.RunConfigVersion)
		}
	})
	t.Run("absent on an older plane decodes to zero", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"version":   7,
			"counts":    map[string]any{"ready": 1},
			"claimable": 1,
		})
		s, err := parseSummary(body)
		if err != nil {
			t.Fatalf("parseSummary: %v", err)
		}
		if s.RunConfigVersion != 0 {
			t.Errorf("RunConfigVersion = %d, want 0 (nothing stored)", s.RunConfigVersion)
		}
	})
}
