package fleet

import (
	"encoding/json"
	"testing"
)

// The beacon's role field (studio-chat.md §2): a planner must not look like a
// worker on the Fleet page, and a report that never mentions a role must keep
// the older wire shape byte for byte.

func TestMarshalReportCarriesTheRole(t *testing.T) {
	b, err := marshalReport(Report{Identity: Identity{Instance: "host/planner", Role: "planner"}})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["role"] != "planner" {
		t.Errorf("role = %v, want planner", got["role"])
	}
}

func TestMarshalReportOmitsAnUnsetRole(t *testing.T) {
	b, err := marshalReport(Report{Identity: Identity{Instance: "host/legacy"}})
	if err != nil {
		t.Fatalf("marshalReport: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["role"]; ok {
		t.Errorf("an unset role reached the wire: %s", b)
	}
}
