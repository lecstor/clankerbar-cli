package plane

// probe.go — proving a credential is live without reading anything that matters.
//
// `clankerbar whoami` needs one authenticated answer from the plane and nothing
// else: whether the key is still accepted. Two lightweight tools cover the two
// shapes an origin serves, and neither reads a task.

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Probe calls the lightest tool the MCP endpoint serves and returns its name:
// `list_projects` on the bare /mcp account surface (CLA-522), falling back to
// `get_backlog_summary` on a project-scoped endpoint, where the project tool set
// does not include list_projects.
//
// Only an unknown-tool refusal falls through: a 401/403, a transport failure or
// a refusal of the call itself is the answer, and retrying a different tool
// would report a healthy key for an endpoint that refused the credential.
func Probe(ctx context.Context, mcpURL, apiKey string) (string, error) {
	if mcpURL == "" || apiKey == "" {
		return "", ErrNotWired
	}
	c := wiredClient(mcpURL, apiKey)
	const first = "list_projects"
	if _, err := c.callText(ctx, first, map[string]any{}); err != nil {
		if Unauthorized(err) || !toolNotServed(first, err) {
			return "", err
		}
		const fallback = "get_backlog_summary"
		if _, ferr := c.callText(ctx, fallback, map[string]any{}); ferr != nil {
			return "", ferr
		}
		return fallback, nil
	}
	return first, nil
}

// Unauthorized reports whether err is the plane refusing the credential itself:
// HTTP 401 or 403. Anything else — a 5xx, a tool refusal, a timeout — is not a
// verdict on the key.
func Unauthorized(err error) bool {
	var status *httpStatusError
	if errors.As(err, &status) {
		return status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden
	}
	return false
}

// toolNotServed reports whether err says the endpoint does not register tool,
// rather than that the tool refused the call. It is the plane package's own
// unknownTool rule applied to a wrapped call error.
func toolNotServed(tool string, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, tool) && unknownTool(tool, msg)
}
