package plane

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// The byte-free download surface (CLA-1014): the read half of the asset path
// CLA-581 opened for uploads. `clankerbar download <assetId>` GETs the plane's
// `clk_`-authenticated asset route and streams the response straight to stdout
// or a file — the bytes never enter a model, a tool argument or this package's
// memory.
//
// This is deliberately NOT an MCP call. The route is a plain HTTP GET
// (`GET /api/projects/<slug>/assets/<assetId>`, CLA-1013) so a shell can stream
// it, and the body is the file itself rather than a JSON envelope around it.

// AssetStream is one opened asset response. The caller must close Body.
type AssetStream struct {
	Body io.ReadCloser

	// ContentType is the verified type the plane stored for these bytes.
	ContentType string
	// ContentLength is the body length the plane declared, or -1 when it sent
	// none (a chunked response). The request asks for identity encoding, so a
	// declared length is the stored file's length rather than a compressed one.
	ContentLength int64
	// Filename is the `Content-Disposition` filename exactly as the plane sent
	// it ("" when absent). It is NOT a safe path: the caller decides where to
	// write and must treat this as a suggestion.
	Filename string
	// SHA256 is the asset's content hash when the plane sent one in a strong
	// ETag (`"<hex>"`), lowercase hex ("" otherwise). The route sets it from the
	// verified row (CLA-1013), so it is an independent check on the bytes.
	SHA256 string
}

// DownloadAPI is the download half of the plane surface. It is separate from
// UploadAPI for the same reason every other capability here is separate: a
// caller handed one cannot type-assert another out of it, and a not-wired plane
// degrades to a named error rather than a nil dereference.
type DownloadAPI interface {
	// Get opens the asset's bytes. A refusal (an invalid key, a non-member, an
	// unknown or pending asset) comes back as the plane's own words, wrapped so
	// the caller can pass them through verbatim. The returned Body must be
	// closed by the caller.
	Get(ctx context.Context) (*AssetStream, error)
}

// NewDownloadAPI builds the download surface for ONE asset URL (built by
// config.ProjectAssetURL). Missing either the URL or the key yields the
// not-wired client, degrading exactly like New and NewUploadAPI.
func NewDownloadAPI(assetURL, apiKey string) DownloadAPI {
	if assetURL == "" || apiKey == "" {
		return downloadNotWired{}
	}
	return &assetClient{
		url:    assetURL,
		apiKey: apiKey,
		// An asset may be a 10 MiB video/document, and the deadline covers the
		// whole body read — the same reasoning as uploadTimeout. Still bounded so
		// a stalled transfer reports instead of hanging a session forever.
		client: &http.Client{Timeout: downloadTimeout, CheckRedirect: sameOriginRedirect(assetURL)},
	}
}

// downloadTimeout bounds a whole asset GET, including reading its body.
const downloadTimeout = 5 * time.Minute

// downloadNotWired is NewDownloadAPI's not-wired answer: only Get, so it cannot
// be asserted into a capability nobody requested.
type downloadNotWired struct{}

func (downloadNotWired) Get(context.Context) (*AssetStream, error) {
	return nil, ErrNotWired
}

type assetClient struct {
	url    string
	apiKey string
	client *http.Client
}

// Get performs the authenticated GET and returns the open response.
//
// The request asks for `Accept-Encoding: identity` on purpose. Go otherwise
// advertises gzip and transparently decompresses, which drops Content-Length and
// makes the streamed bytes a decoded copy: the hash check would then be against
// the wrong bytes, and a truncated file would pass. Assets are already-compressed
// media or small documents; asking for the stored bytes verbatim keeps the length
// and the hash honest.
func (a *assetClient) Get(ctx context.Context) (*AssetStream, error) {
	// The URL was built from CredentialOrigin, but validate it anyway: this
	// client can also be constructed directly, and the key rides on the request.
	if _, err := secureurl.Origin(a.url); err != nil {
		return nil, fmt.Errorf("download: unsafe asset URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	// Only 200 is a complete asset from this route. A 206 can only be a partial
	// body (this client sends no Range), and treating any other 2xx as success
	// would write a truncated file that no declared length accounts for.
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, downloadRefusal(resp.StatusCode, raw)
	}

	stream := &AssetStream{
		Body:          resp.Body,
		ContentType:   resp.Header.Get("Content-Type"),
		ContentLength: resp.ContentLength,
		Filename:      dispositionFilename(resp.Header.Get("Content-Disposition")),
		SHA256:        etagSHA256(resp.Header.Get("ETag")),
	}
	return stream, nil
}

// downloadRefusal renders a rejected GET from the plane's own words. The route's
// authorization failures are JSON (`{"error":{"code","message"}}`) and its one
// not-found is a plain "Not found"; both must reach the caller verbatim, because
// the plane is the only side that knows which of them it meant.
func downloadRefusal(status int, raw []byte) error {
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &wire); err == nil && wire.Error.Message != "" {
		if code := strings.TrimSpace(wire.Error.Code); code != "" {
			return fmt.Errorf("download refused: HTTP %d %s: %s", status, code, wire.Error.Message)
		}
		return fmt.Errorf("download refused: HTTP %d: %s", status, wire.Error.Message)
	}
	return fmt.Errorf("download refused: HTTP %d: %s", status, strings.TrimSpace(string(raw)))
}

// dispositionFilename pulls the filename out of a Content-Disposition header,
// RFC 6266 form (`attachment; filename="report.md"`), including the
// `filename*=UTF-8”...` spelling mime.ParseMediaType decodes. Anything
// unparseable yields "".
func dispositionFilename(header string) string {
	if strings.TrimSpace(header) == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// etagSHA256 returns the lowercase content hash from a strong ETag, or "" when
// the header is absent, weak, or not a sha256. Only the shape the asset route
// promises (`"<64 hex>"`) is accepted, so an ETag that means something else
// (another deployment's md5, a quoted version tag) can never be mistaken for a
// mismatch and fail a legitimate download.
func etagSHA256(etag string) string {
	etag = strings.TrimSpace(etag)
	// A weak validator is explicitly not a byte-for-byte assertion.
	if strings.HasPrefix(etag, "W/") || strings.HasPrefix(etag, "w/") {
		return ""
	}
	etag = strings.Trim(etag, `"`)
	if len(etag) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(etag); err != nil {
		return ""
	}
	return strings.ToLower(etag)
}
