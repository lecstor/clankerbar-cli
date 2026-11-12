package plane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The refusal codes that mean the run is gone. Each one is a different plane-side
// reason (taken over, reclaimed, released, withheld, bad credential) and all of
// them answer the same question: this session is working a task that is not its
// own, so the renewer must end it rather than retry.
func TestHeartbeatRefusalThatMeansTheRunIsGone(t *testing.T) {
	ctx := context.Background()
	codes := []string{
		"not_your_run",
		"run_superseded",
		"is_container",
		"human_only",
		"not_claimable",
		"run_secret_required",
		"run_secret_invalid",
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, refusalBody(t, code, "the plane says "+code))

			err := mustClient(t, srv).Heartbeat(ctx, "r-1")
			if !errors.Is(err, ErrRunLost) {
				t.Fatalf("err = %v, want ErrRunLost", err)
			}
			// The plane's own wording is what tells the operator what happened.
			if !strings.Contains(err.Error(), "the plane says "+code) {
				t.Errorf("err = %v, want the plane's message carried", err)
			}
		})
	}
}

// Everything else is not "the run is gone": a transport failure, a code this
// build does not name, a broken response. Classifying one of those as ErrRunLost
// would kill a session that still holds its task.
func TestHeartbeatRefusalsItCannotClassifyPassThrough(t *testing.T) {
	ctx := context.Background()

	t.Run("unnamed code", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, refusalBody(t, "internal_error", "boom"))

		err := mustClient(t, srv).Heartbeat(ctx, "r-1")
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the plane's refusal carried through", err)
		}
		if errors.Is(err, ErrRunLost) {
			t.Errorf("err = %v was classified as a lost run on a code that does not say so", err)
		}
	})

	t.Run("json-rpc error", func(t *testing.T) {
		srv, _ := serve(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"message":"boom"}}`)

		err := mustClient(t, srv).Heartbeat(ctx, "r-1")
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the transport failure carried through", err)
		}
		if errors.Is(err, ErrRunLost) {
			t.Errorf("err = %v was classified as a lost run on a transport failure", err)
		}
	})

	t.Run("unreadable response", func(t *testing.T) {
		srv, _ := serve(t, http.StatusInternalServerError, "boom")

		err := mustClient(t, srv).Heartbeat(ctx, "r-1")
		if err == nil {
			t.Fatal("Heartbeat returned nil on an unreadable response")
		}
		if errors.Is(err, ErrRunLost) {
			t.Errorf("err = %v was classified as a lost run on a decode failure", err)
		}
	})

	t.Run("not wired", func(t *testing.T) {
		if err := (notWired{}).Heartbeat(ctx, "r-1"); !errors.Is(err, ErrNotWired) {
			t.Fatalf("err = %v, want ErrNotWired", err)
		}
	})
}
