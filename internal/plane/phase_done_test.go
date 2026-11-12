package plane

// phase_done_test.go — the per-phase completion marker: it goes out as
// record_progress({phaseDone}), never carries a status, and values the plane
// would refuse are refused here so they never cost a round-trip.

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestRecordPhaseDoneSendsTheMarker(t *testing.T) {
	srv, got := serve(t, http.StatusOK, okBody)

	if err := mustClient(t, srv).RecordPhaseDone(context.Background(), "task-1", "run-1", "implement", "0123abcd"); err != nil {
		t.Fatalf("RecordPhaseDone: %v", err)
	}

	if name := toolName(t, got.body); name != "record_progress" {
		t.Errorf("tool = %q, want record_progress", name)
	}
	args := toolArgs(t, got.body)
	want := map[string]any{
		"taskId":    "task-1",
		"runId":     "run-1",
		"phaseDone": map[string]any{"phase": "implement", "tip": "0123abcd"},
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("arguments = %+v, want %+v", args, want)
	}
	// A marker revises a record; any of these would make it a status move or a
	// delivery claim.
	for _, forbidden := range []string{"branch", "outcome", "delivery", "status"} {
		if _, present := args[forbidden]; present {
			t.Errorf("the marker must not carry %q: %+v", forbidden, args)
		}
	}
}

// The plane refuses these, so the client refuses them first: a bad value must
// never reach the wire.
func TestRecordPhaseDoneRefusesLocally(t *testing.T) {
	cases := []struct {
		name   string
		taskID string
		runID  string
		phase  string
		tip    string
	}{
		{name: "empty task id", taskID: "", runID: "run-1", phase: "implement", tip: "0123abcd"},
		{name: "empty run id", taskID: "task-1", runID: "", phase: "implement", tip: "0123abcd"},
		{name: "uppercase phase", taskID: "task-1", runID: "run-1", phase: "Implement", tip: "0123abcd"},
		{name: "phase with a space", taskID: "task-1", runID: "run-1", phase: "re view", tip: "0123abcd"},
		{name: "empty phase", taskID: "task-1", runID: "run-1", phase: "", tip: "0123abcd"},
		{name: "phase too long", taskID: "task-1", runID: "run-1", phase: strings.Repeat("a", 41), tip: "0123abcd"},
		{name: "tip too short", taskID: "task-1", runID: "run-1", phase: "implement", tip: "abc"},
		{name: "uppercase tip", taskID: "task-1", runID: "run-1", phase: "implement", tip: "0123ABCD"},
		{name: "empty tip", taskID: "task-1", runID: "run-1", phase: "implement", tip: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, okBody)

			if err := mustClient(t, srv).RecordPhaseDone(context.Background(), tc.taskID, tc.runID, tc.phase, tc.tip); err == nil {
				t.Fatal("a value the plane would refuse must be refused locally")
			}
			if len(got.bodies) != 0 {
				t.Errorf("sent %d request(s), want none: %+v", len(got.bodies), got.bodies)
			}
		})
	}
}

func TestRecordPhaseDoneSurfacesARefusal(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, refusalBody(t, "run_superseded", "a later run owns this task"))

	if err := mustClient(t, srv).RecordPhaseDone(context.Background(), "task-1", "run-1", "implement", "0123abcd"); err == nil {
		t.Fatal("a refused marker must be an error: the plane did not record it")
	}
}
