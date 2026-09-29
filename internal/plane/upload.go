package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The byte-free upload surface (CLA-581): the CLI declares a file with three
// cheap scalars over MCP, then PUTs the bytes straight at the plane from its own
// process. Nothing in this path ever sees file content — that is the point of
// it. Before CLA-578 the only route into the asset store was a data: URI a model
// typed by hand, which is how MAK-123 emitted 7,076 characters of base64 by
// hand, burned a whole step on it and never made the call.
//
// This reuses the same mcpReleaser (endpoint, bearer key, JSON-RPC transport)
// the driver's writes use — deliberately, so uploads cannot drift from the rest
// of the client's auth or framing.

// CreateUploadRequest is the declaration the plane holds the sender to: the
// content type and size are checked against its allowlist and caps, and the
// sha256 is what the PUT verifies the received bytes against.
type CreateUploadRequest struct {
	ContentType string
	SizeBytes   int64
	SHA256      string

	// TaskID is optional ATTRIBUTION only (a CLA-12 ref or id). The plane
	// resolves it and records who the file belongs to; it changes no gate.
	TaskID string
}

// UploadTicket is what create_upload handed back. UploadURL is the single-use,
// token-authenticated PUT target; AlreadyStored is the plane's "these exact
// bytes are already in this project" answer, in which case no URL is returned
// and the asset id is the whole result.
type UploadTicket struct {
	AssetID       string
	UploadURL     string
	ExpiresAt     string
	AlreadyStored bool
}

// UploadAPI is the upload half of the plane surface. It is a separate interface
// for the same reason Releaser/Recorder/ParkAPI are: a caller handed one
// capability cannot type-assert another out of it, and a not-wired plane
// degrades to a named error rather than a nil dereference.
type UploadAPI interface {
	// CreateUpload declares a file and returns its ticket. A refusal (unknown
	// type, over cap, rate limit, unclaimed project) comes back as the plane's
	// own words, wrapped so the caller can pass them through verbatim.
	CreateUpload(ctx context.Context, req CreateUploadRequest) (UploadTicket, error)

	// PutUpload sends the bytes to a ticket's upload URL. The URL's token IS
	// the auth — no API key is sent to this route, and none must be: the plane
	// deliberately accepts the PUT with no other credential.
	PutUpload(ctx context.Context, uploadURL string, body io.Reader, sizeBytes int64, contentType string) error

	// UploadReviewDeck binds a READY text/html asset to a task as its review
	// deck and returns the review URL the task now points at.
	UploadReviewDeck(ctx context.Context, taskID, assetID string) (string, error)
}

// NewUploadAPI builds the upload surface. Missing either the endpoint or the
// key yields the not-wired client, degrading exactly like New.
//
// mcpURL is the project-scoped MCP endpoint (`https://…/mcp/<slug>`).
func NewUploadAPI(mcpURL, apiKey string) UploadAPI {
	if mcpURL == "" || apiKey == "" {
		return uploadNotWired{}
	}
	return wiredClient(mcpURL, apiKey)
}

// uploadNotWired is NewUploadAPI's not-wired answer — only what UploadAPI
// names, so it cannot be asserted into a capability nobody requested.
type uploadNotWired struct{}

func (uploadNotWired) CreateUpload(context.Context, CreateUploadRequest) (UploadTicket, error) {
	return UploadTicket{}, ErrNotWired
}

func (uploadNotWired) PutUpload(context.Context, string, io.Reader, int64, string) error {
	return ErrNotWired
}

func (uploadNotWired) UploadReviewDeck(context.Context, string, string) (string, error) {
	return "", ErrNotWired
}

// uploadTimeout bounds a PUT. It is far longer than the MCP calls' 20s because
// this is the one request that carries a whole file: a 10 MiB video on a home
// uplink can take minutes, and a deadline it cannot meet would fail a legitimate
// upload with nothing the caller could do differently. Still bounded, because an
// unbounded PUT would hang a session instead of reporting.
const uploadTimeout = 5 * time.Minute

// CreateUpload calls the plane's create_upload.
func (r *mcpReleaser) CreateUpload(ctx context.Context, req CreateUploadRequest) (UploadTicket, error) {
	if req.ContentType == "" || req.SizeBytes <= 0 || req.SHA256 == "" {
		return UploadTicket{}, errors.New("create upload: contentType, sizeBytes and sha256 are all required")
	}
	args := map[string]any{
		"contentType": req.ContentType,
		"sizeBytes":   req.SizeBytes,
		"sha256":      req.SHA256,
	}
	if req.TaskID != "" {
		args["taskId"] = req.TaskID
	}
	raw, err := r.callText(ctx, "create_upload", args)
	if err != nil {
		return UploadTicket{}, err
	}
	var wire struct {
		AssetID   string `json:"assetId"`
		UploadURL string `json:"uploadUrl"`
		ExpiresAt string `json:"expiresAt"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return UploadTicket{}, fmt.Errorf("create_upload: decode response: %w", err)
	}
	if wire.AssetID == "" {
		return UploadTicket{}, errors.New("create_upload: the plane returned no assetId")
	}
	return UploadTicket{
		AssetID:   wire.AssetID,
		UploadURL: wire.UploadURL,
		ExpiresAt: wire.ExpiresAt,
		// The already-stored answer carries `status: "ready"` and no URL. Both
		// are read: a "ready" asset is never re-uploaded, and a response with no
		// URL has nothing to PUT to either way.
		AlreadyStored: wire.Status == "ready" || wire.UploadURL == "",
	}, nil
}

// PutUpload sends the bytes to the ticket's URL. The body is the caller's
// stream — a file handle, never bytes this process assembled into memory — and
// the declared length rides Content-Length so the route can refuse a wrong size
// before it receives anything.
//
// This intentionally sends NO Authorization header. The URL's token is the
// whole of the authorization, and the account key must not travel to a route
// that deliberately accepts no other credential.
func (r *mcpReleaser) PutUpload(ctx context.Context, uploadURL string, body io.Reader, sizeBytes int64, contentType string) error {
	if uploadURL == "" {
		return errors.New("upload PUT: no upload URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return fmt.Errorf("upload PUT: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = sizeBytes

	client := r.upload
	if client == nil {
		client = &http.Client{Timeout: uploadTimeout, CheckRedirect: noDowngradeRedirect}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload PUT: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return putRefusal(resp.StatusCode, raw)
	}
	return nil
}

// putRefusal renders a rejected PUT from the plane's own error shape
// (`{"error":{"code","message"}}`), keeping the code so a 401/403 auth failure
// is never mistaken for a hash or size mismatch. Anything that does not parse
// still names the HTTP status and whatever the body said.
func putRefusal(status int, raw []byte) error {
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &wire); err == nil && wire.Error.Message != "" {
		if code := strings.TrimSpace(wire.Error.Code); code != "" {
			return fmt.Errorf("upload PUT refused: HTTP %d %s: %s", status, code, wire.Error.Message)
		}
		return fmt.Errorf("upload PUT refused: HTTP %d: %s", status, wire.Error.Message)
	}
	return fmt.Errorf("upload PUT refused: HTTP %d: %s", status, strings.TrimSpace(string(raw)))
}

// UploadReviewDeck calls the plane's upload_review_deck with the asset form:
// the deck HTML came through the upload path, and the plane links the ready
// text/html asset to the task. Returns the review URL the task now points at.
func (r *mcpReleaser) UploadReviewDeck(ctx context.Context, taskID, assetID string) (string, error) {
	if taskID == "" || assetID == "" {
		return "", errors.New("upload review deck: taskId and assetId are both required")
	}
	raw, err := r.callText(ctx, "upload_review_deck", map[string]any{
		"taskId":  taskID,
		"assetId": assetID,
	})
	if err != nil {
		return "", err
	}
	var wire struct {
		ReviewURL string `json:"reviewUrl"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return "", fmt.Errorf("upload_review_deck: decode response: %w", err)
	}
	if wire.ReviewURL == "" {
		return "", errors.New("upload_review_deck: the plane returned no reviewUrl")
	}
	return wire.ReviewURL, nil
}
