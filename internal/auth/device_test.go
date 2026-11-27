package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/auth"
)

// devicePlane is the two public device routes, scripted per poll.
type devicePlane struct {
	srv *httptest.Server

	mu         sync.Mutex
	codeBody   map[string]any
	tokenPolls []map[string]any
	token      []deviceTokenResponse
	codeReqs   []map[string]any
}

type deviceTokenResponse struct {
	status int
	body   map[string]any
}

func newDevicePlane(t *testing.T) *devicePlane {
	t.Helper()
	p := &devicePlane{
		codeBody: map[string]any{
			"device_code":               "device-code-123",
			"user_code":                 "WDJB-MJHT",
			"verification_uri":          "https://clankerbar.com/device",
			"verification_uri_complete": "https://clankerbar.com/device?user_code=WDJB-MJHT",
			"expires_in":                600,
			"interval":                  1,
		},
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)

		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.URL.Path {
		case "/api/device/code":
			p.codeReqs = append(p.codeReqs, req)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p.codeBody)
		case "/api/device/token":
			p.tokenPolls = append(p.tokenPolls, req)
			if len(p.token) == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			next := p.token[0]
			p.token = p.token[1:]
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(next.status)
			_ = json.NewEncoder(w).Encode(next.body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// queue appends token responses to serve, in order.
func (p *devicePlane) queue(responses ...deviceTokenResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = append(p.token, responses...)
}

func (p *devicePlane) codeRequests() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.codeReqs...)
}

func (p *devicePlane) polls() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.tokenPolls...)
}

func noSleep(context.Context, time.Duration) error { return nil }

// recordSleep records each requested wait, so the interval and slow_down rules
// are asserted rather than timed.
func recordSleep(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
}

func TestRequestDeviceCodeSendsScopeAndClientName(t *testing.T) {
	p := newDevicePlane(t)
	grant, err := auth.RequestDeviceCode(context.Background(), p.srv.URL, auth.ScopeUser)
	if err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}
	if grant.DeviceCode != "device-code-123" || grant.UserCode != "WDJB-MJHT" {
		t.Errorf("grant = %+v", grant)
	}
	reqs := p.codeRequests()
	if len(reqs) != 1 {
		t.Fatalf("code requests = %d, want 1", len(reqs))
	}
	if reqs[0]["scope"] != auth.ScopeUser || reqs[0]["client_name"] != auth.ClientName {
		t.Errorf("code request = %v, want scope=user client_name=%s", reqs[0], auth.ClientName)
	}
}

func TestRequestDeviceCodeProjectScope(t *testing.T) {
	p := newDevicePlane(t)
	if _, err := auth.RequestDeviceCode(context.Background(), p.srv.URL, auth.ScopeProject); err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}
	reqs := p.codeRequests()
	if len(reqs) != 1 || reqs[0]["scope"] != auth.ScopeProject {
		t.Errorf("code request = %v, want scope=project", reqs)
	}
}

func TestPollDeviceTokenHappyPath(t *testing.T) {
	p := newDevicePlane(t)
	p.queue(
		deviceTokenResponse{status: http.StatusBadRequest, body: map[string]any{"error": "authorization_pending"}},
		deviceTokenResponse{status: http.StatusOK, body: map[string]any{
			"access_token": testToken,
			"scope":        "user",
			"project_slug": "acme",
		}},
	)
	var waits []time.Duration
	token, err := auth.PollDeviceToken(context.Background(), p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 2},
		auth.PollOptions{Sleep: recordSleep(&waits)})
	if err != nil {
		t.Fatalf("PollDeviceToken: %v", err)
	}
	if token.AccessToken != testToken || token.Scope != "user" || token.ProjectSlug != "acme" {
		t.Errorf("token = %+v", token)
	}
	if len(waits) != 2 || waits[0] != 2*time.Second || waits[1] != 2*time.Second {
		t.Errorf("waits = %v, want two 2s waits inside the grant's interval", waits)
	}
	polls := p.polls()
	if len(polls) != 2 || polls[0]["device_code"] != "device-code-123" {
		t.Errorf("polls = %v, want the grant's device_code", polls)
	}
}

func TestPollDeviceTokenSlowDownBacksOff(t *testing.T) {
	p := newDevicePlane(t)
	p.queue(
		deviceTokenResponse{status: http.StatusBadRequest, body: map[string]any{"error": "slow_down"}},
		deviceTokenResponse{status: http.StatusOK, body: map[string]any{"access_token": testToken, "scope": "user"}},
	)
	var waits []time.Duration
	if _, err := auth.PollDeviceToken(context.Background(), p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 2},
		auth.PollOptions{Sleep: recordSleep(&waits)}); err != nil {
		t.Fatalf("PollDeviceToken: %v", err)
	}
	if len(waits) != 2 || waits[1] != waits[0]+5*time.Second {
		t.Errorf("waits = %v, want the second to be the first + 5s", waits)
	}
}

func TestPollDeviceTokenAccessDenied(t *testing.T) {
	p := newDevicePlane(t)
	p.queue(deviceTokenResponse{status: http.StatusBadRequest, body: map[string]any{
		"error": "access_denied", "error_description": "The operator denied this request.",
	}})
	_, err := auth.PollDeviceToken(context.Background(), p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 1},
		auth.PollOptions{Sleep: noSleep})
	var flow *auth.DeviceFlowError
	if !errors.As(err, &flow) || flow.Code != "access_denied" {
		t.Fatalf("PollDeviceToken error = %v, want access_denied", err)
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("the refusal does not carry the plane's description: %v", err)
	}
}

func TestPollDeviceTokenExpired(t *testing.T) {
	p := newDevicePlane(t)
	p.queue(deviceTokenResponse{status: http.StatusBadRequest, body: map[string]any{"error": "expired_token"}})
	_, err := auth.PollDeviceToken(context.Background(), p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 1},
		auth.PollOptions{Sleep: noSleep})
	var flow *auth.DeviceFlowError
	if !errors.As(err, &flow) || flow.Code != "expired_token" {
		t.Fatalf("PollDeviceToken error = %v, want expired_token", err)
	}
	if !strings.Contains(err.Error(), "clankerbar login") {
		t.Errorf("the expiry message should name the fix: %v", err)
	}
}

func TestPollDeviceTokenHonoursContextCancellation(t *testing.T) {
	p := newDevicePlane(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := auth.PollDeviceToken(ctx, p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 1},
		auth.PollOptions{Sleep: noSleep})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PollDeviceToken on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestPollDeviceTokenGivesUpAtExpiry(t *testing.T) {
	p := newDevicePlane(t)
	// No scripted response: the server answers authorization_pending. The only
	// thing that can end the poll is the grant's own expires_in.
	p.queue()
	_, err := auth.PollDeviceToken(context.Background(), p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 1, ExpiresIn: 1},
		auth.PollOptions{})
	var flow *auth.DeviceFlowError
	if !errors.As(err, &flow) || flow.Code != "expired_token" {
		t.Fatalf("PollDeviceToken past expiry = %v, want expired_token", err)
	}
}

func TestPollDeviceTokenRefusesAnEmptyAccessToken(t *testing.T) {
	p := newDevicePlane(t)
	p.queue(deviceTokenResponse{status: http.StatusOK, body: map[string]any{"scope": "user"}})
	_, err := auth.PollDeviceToken(context.Background(), p.srv.URL,
		auth.DeviceAuthorization{DeviceCode: "device-code-123", Interval: 1},
		auth.PollOptions{Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "no access_token") {
		t.Fatalf("PollDeviceToken without a token = %v", err)
	}
}

func TestRequestDeviceCodeRefusesAnUnsafeOrigin(t *testing.T) {
	if _, err := auth.RequestDeviceCode(context.Background(), "http://attacker.example", auth.ScopeUser); err == nil {
		t.Fatal("RequestDeviceCode accepted plain http to a non-loopback host")
	}
}
