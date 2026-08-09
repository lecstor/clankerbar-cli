package harness

import "testing"

// Capturing the delivery claims the driver then checks against local git
// (CLA-253). The discipline mirrors claim tracking exactly: a claim is kept only
// if the PLANE accepted the call that made it, because a refused `update_task`
// recorded nothing and complaining about it would be noise.

func TestClaudeDeliveryReports(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []Report
	}{
		{
			// The hand-off record. CLA-134 recorded one of these and never pushed
			// past its first three commits.
			name: "a recorded branch is a claim worth checking",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","branch":"clanker/x"}`),
				toolResult("u2", `{}`),
			),
			want: []Report{{TaskID: claimUUID, Ref: claimRef, RunID: "r-1", Branch: "clanker/x"}},
		},
		{
			name: "a declared delivery carries the ancestor check the plane only stores",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"done","delivery":{"commit":"abc123","integrationBranch":"main","mergeVerified":true}}`),
				toolResult("u2", `{}`),
			),
			want: []Report{{
				TaskID: claimUUID, Ref: claimRef, RunID: "r-1", Status: "done",
				Commit: "abc123", IntegrationBranch: "main",
			}},
		},
		{
			name: "branch and delivery on one call is one report",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimRef+`","status":"in_review","branch":"clanker/x","delivery":{"commit":"abc123","integrationBranch":"main"}}`),
				toolResult("u2", `{}`),
			),
			want: []Report{{
				TaskID: claimUUID, Ref: claimRef, RunID: "r-1", Status: "in_review",
				Branch: "clanker/x", Commit: "abc123", IntegrationBranch: "main",
			}},
		},
		{
			// The plane refused, so nothing was recorded. Checking it would produce a
			// complaint about a hand-off that does not exist.
			name: "a REFUSED update claims nothing",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","branch":"clanker/x"}`),
				toolResultErr("u2", "run_superseded"),
			),
			want: nil,
		},
		{
			name: "an update that claims no delivery is not a report",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"in_progress","outcome":"still going"}`),
				toolResult("u2", `{}`),
			),
			want: nil,
		},
		{
			// A delivery naming no integration branch cannot be traced to one, so
			// there is nothing to check — not a silent pass, simply not a claim.
			name: "a commit with no integration branch is not checkable",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"done","delivery":{"commit":"abc123"}}`),
				toolResult("u2", `{}`),
			),
			want: nil,
		},
		{
			// Same discipline as claim tracking: an update naming SOMEBODY ELSE'S
			// task says nothing about the tree this session worked in.
			name: "an update to another task is not this session's delivery",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"CLA-999","branch":"clanker/other"}`),
				toolResult("u2", `{}`),
			),
			want: nil,
		},
		{
			name: "several deliveries are kept in order",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","branch":"clanker/x"}`),
				toolResult("u2", `{}`),
				toolUse("u3", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"in_review","delivery":{"commit":"abc123","integrationBranch":"main"}}`),
				toolResult("u3", `{}`),
			),
			want: []Report{
				{TaskID: claimUUID, Ref: claimRef, RunID: "r-1", Branch: "clanker/x"},
				{TaskID: claimUUID, Ref: claimRef, RunID: "r-1", Status: "in_review", Commit: "abc123", IntegrationBranch: "main"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claimStream(tc.lines...).Reports
			if len(got) != len(tc.want) {
				t.Fatalf("Reports = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Reports[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestReportEmptyAndLabel(t *testing.T) {
	if !(Report{}).Empty() {
		t.Error("a report claiming nothing is empty")
	}
	if (Report{Branch: "b"}).Empty() {
		t.Error("a recorded branch is a claim")
	}
	if !(Report{Commit: "c"}).Empty() {
		t.Error("a commit with nothing to trace it to is not checkable")
	}
	if got := (Report{TaskID: "uuid", Ref: "CLA-1"}).Label(); got != "CLA-1" {
		t.Errorf("Label() = %q, want the ref", got)
	}
	if got := (Report{TaskID: "uuid"}).Label(); got != "uuid" {
		t.Errorf("Label() = %q, want the id when there is no ref", got)
	}
}
