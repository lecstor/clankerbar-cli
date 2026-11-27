package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// The browser device flow (RFC 8628) the plane serves at these two public
// routes. `clankerbar login` is the only caller.
const (
	deviceCodePath  = "/api/device/code"
	deviceTokenPath = "/api/device/token"

	// ClientName is what the grant's record says asked for the key. A
	// recognisable name on the operator's /device approval page is the whole
	// point of sending it.
	ClientName = "clankerbar-cli"

	defaultPollInterval = 5 * time.Second
	slowDownIncrement   = 5 * time.Second
	maxTransientPolls   = 5
	maxDeviceBodyBytes  = 1 << 20

	// defaultGrantLifetime bounds a poll whose grant omitted expires_in, so a
	// plane that never answers cannot keep `login` waiting forever.
	defaultGrantLifetime = 10 * time.Minute
)

// deviceHTTPClient bounds one request. Login is interactive: a hung plane
// should surface as an error, not a command that never returns.
var deviceHTTPClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: secureurl.NoDowngradeRedirect,
}

// DeviceAuthorization is the grant POST /api/device/code issues.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceToken is the credential POST /api/device/token releases once a human
// approves the grant. Scope and ProjectSlug are the CLA-1037 additions; a plane
// that predates them leaves them empty.
type DeviceToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	ProjectSlug string `json:"project_slug"`
}

// DeviceFlowError is one RFC 8628 error response: a state the flow ends in, as
// opposed to a transport failure a retry could fix.
type DeviceFlowError struct {
	Code        string
	Description string
}

func (e *DeviceFlowError) Error() string {
	msg := strings.TrimSpace(e.Description)
	switch e.Code {
	case "access_denied":
		if msg == "" {
			msg = "the request was denied on the device page"
		}
	case "expired_token":
		if msg == "" {
			msg = "the device code expired before it was approved; run `clankerbar login` again"
		}
	}
	if msg == "" {
		return "device flow: " + e.Code
	}
	return "device flow: " + msg + " (" + e.Code + ")"
}

// RequestDeviceCode starts a device grant for scope at origin.
func RequestDeviceCode(ctx context.Context, origin, scope string) (DeviceAuthorization, error) {
	norm, err := canonicalOrigin(origin)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	if scope == "" {
		scope = ScopeUser
	}
	if scope != ScopeUser && scope != ScopeProject {
		return DeviceAuthorization{}, fmt.Errorf("device flow: %q is not a known scope (%q or %q)", scope, ScopeUser, ScopeProject)
	}
	status, raw, err := postJSON(ctx, norm+deviceCodePath, map[string]string{
		"scope":       scope,
		"client_name": ClientName,
	})
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("request a device code from %s: %w", norm, err)
	}
	var payload struct {
		DeviceAuthorization
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return DeviceAuthorization{}, fmt.Errorf("request a device code from %s: HTTP %d: the response was not valid JSON", norm, status)
	}
	if payload.Error != "" {
		return DeviceAuthorization{}, &DeviceFlowError{Code: payload.Error, Description: payload.ErrorDescription}
	}
	if status != http.StatusOK {
		return DeviceAuthorization{}, fmt.Errorf("request a device code from %s: HTTP %d", norm, status)
	}
	if payload.DeviceCode == "" || payload.UserCode == "" {
		return DeviceAuthorization{}, fmt.Errorf("request a device code from %s: the response carried no device_code/user_code", norm)
	}
	return payload.DeviceAuthorization, nil
}

// PollOptions tunes PollDeviceToken. Sleep is the test seam: production leaves
// it nil and waits on a real timer that respects ctx.
type PollOptions struct {
	Sleep func(context.Context, time.Duration) error
}

// PollDeviceToken polls the token endpoint until the grant is approved, denied
// or expired. It respects the grant's interval, backs off by 5s on slow_down
// (RFC 8628 §3.5), and gives up when ctx is done, the grant's expires_in has
// passed, or a bounded default lifetime passes when the grant omitted expires_in.
// A response body is never echoed in an error: the token endpoint's body carries
// the freshly minted access_token.
func PollDeviceToken(ctx context.Context, origin string, grant DeviceAuthorization, opts PollOptions) (DeviceToken, error) {
	norm, err := canonicalOrigin(origin)
	if err != nil {
		return DeviceToken{}, err
	}
	if grant.DeviceCode == "" {
		return DeviceToken{}, fmt.Errorf("poll %s: the device grant carries no device_code", norm)
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	interval := time.Duration(grant.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	deadline := time.Now().Add(defaultGrantLifetime)
	if grant.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(grant.ExpiresIn) * time.Second)
	}
	transient := 0

	for {
		if err := ctx.Err(); err != nil {
			return DeviceToken{}, err
		}
		if time.Now().After(deadline) {
			return DeviceToken{}, &DeviceFlowError{Code: "expired_token"}
		}
		if err := sleep(ctx, interval); err != nil {
			return DeviceToken{}, err
		}

		status, raw, err := postJSON(ctx, norm+deviceTokenPath, map[string]string{"device_code": grant.DeviceCode})
		if err != nil {
			transient++
			if transient >= maxTransientPolls {
				return DeviceToken{}, fmt.Errorf("poll %s: %w", norm, err)
			}
			continue
		}
		var payload struct {
			AccessToken      string `json:"access_token"`
			Scope            string `json:"scope"`
			ProjectSlug      string `json:"project_slug"`
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			if transientStatus(status) {
				transient++
				if transient >= maxTransientPolls {
					return DeviceToken{}, fmt.Errorf("poll %s: HTTP %d: the response was not valid JSON", norm, status)
				}
				continue
			}
			return DeviceToken{}, fmt.Errorf("poll %s: HTTP %d: the response was not valid JSON", norm, status)
		}
		switch payload.Error {
		case "":
			if status != http.StatusOK {
				if transientStatus(status) {
					transient++
					if transient < maxTransientPolls {
						continue
					}
				}
				return DeviceToken{}, fmt.Errorf("poll %s: HTTP %d", norm, status)
			}
			if payload.AccessToken == "" {
				return DeviceToken{}, fmt.Errorf("poll %s: the plane approved the request but returned no access_token", norm)
			}
			return DeviceToken{
				AccessToken: payload.AccessToken,
				Scope:       payload.Scope,
				ProjectSlug: payload.ProjectSlug,
			}, nil
		case "authorization_pending":
			transient = 0
		case "slow_down":
			transient = 0
			interval += slowDownIncrement
		default:
			// access_denied, expired_token, invalid_grant, anything new: a state
			// the flow ends in. Returning the plane's own words is what lets an
			// operator tell "denied" from "already used".
			return DeviceToken{}, &DeviceFlowError{Code: payload.Error, Description: payload.ErrorDescription}
		}
	}
}

// sleepContext waits d, or returns early when ctx is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func transientStatus(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

// postJSON sends one JSON body and returns the status and raw response body.
func postJSON(ctx context.Context, endpoint string, body any) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := deviceHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDeviceBodyBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}
