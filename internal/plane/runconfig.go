// Run-config reads and proposals over MCP (CLA-410): the driver's view of the
// plane surface CLA-408 shipped. The read is the edit signal's other half —
// the backlog-summary poll carries `runConfigVersion` for free, and this
// fetches the document itself when that version moves. The propose call is
// `propose-config`'s transport: it records a PENDING proposal only; there is
// deliberately no clanker-reachable ratify anywhere on the wire.
package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoConfig is not an error condition the caller reports: it is the plane's
// "this project never stored a config" answer (version 0), handed back as a
// typed value so the caller can fall back to local rules without string
// matching.
var ErrNoConfig = errors.New("no run config stored for this project")

// RunConfigState is one get_project_run_config answer: the stored document
// (raw JSON — its keys are the CLI's own config names, decoded by
// config.RunConfigDoc) and the version the backlog-summary poll compares
// against.
type RunConfigState struct {
	Version int
	Config  json.RawMessage
	// Pending is true when a proposal awaits the operator's ratification.
	// Informational only — the loop runs on the EFFECTIVE document either way.
	Pending bool
}

// RunConfigReader fetches this project's effective execution config.
type RunConfigReader interface {
	RunConfig(ctx context.Context) (*RunConfigState, error)
}

// Proposer records a pending execution-config proposal (the operator still has
// to ratify it in the console). Separate interface, like Releaser's siblings,
// so a caller holds exactly the capability it needs.
type Proposer interface {
	ProposeRunConfig(ctx context.Context, doc map[string]any, notes string) error
}

// RunConfigAPI is the read-and-propose half of the execution-config surface —
// what `propose-config` and a run-config-aware target each hold.
type RunConfigAPI interface {
	RunConfigReader
	Proposer
}

// rcNotWired is NewRunConfigAPI's not-wired answer; the methods live on it in
// plane.go beside notWired's, and are not repeated here.

// RunConfig fetches get_project_run_config. A project that never stored one
// answers version 0 with the empty default document; both facts are collapsed
// into ErrNoConfig so the caller's fallback branch is one comparison.
func (r *mcpReleaser) RunConfig(ctx context.Context) (*RunConfigState, error) {
	text, err := r.callText(ctx, "get_project_run_config", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Version int             `json:"version"`
		Config  json.RawMessage `json:"config"`
		Pending struct {
			ID json.RawMessage `json:"id"`
		} `json:"pendingProposal"`
	}
	if err := json.Unmarshal(text, &payload); err != nil {
		return nil, fmt.Errorf("get_project_run_config: decode result: %w", err)
	}
	if payload.Version == 0 {
		return nil, ErrNoConfig
	}
	return &RunConfigState{
		Version: payload.Version,
		Config:  payload.Config,
		Pending: len(payload.Pending.ID) > 0 && string(payload.Pending.ID) != "null",
	}, nil
}

// ProposeRunConfig proposes ONE full validated document. Notes carry the
// derivation rationale for the operator's ratify card; the plane text-repairs
// them like any agent-authored stored prose.
func (r *mcpReleaser) ProposeRunConfig(ctx context.Context, doc map[string]any, notes string) error {
	if len(doc) == 0 {
		return errors.New("propose run config: refusing an empty document")
	}
	args := map[string]any{"config": doc}
	if notes != "" {
		args["notes"] = notes
	}
	return r.call(ctx, "propose_project_run_config", args)
}
