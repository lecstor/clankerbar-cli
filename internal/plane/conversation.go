package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// The conversation surface (studio-chat.md §3/§4, CLA-920): what the planner
// loop calls once per turn. Both are MCP tool calls, unlike the 2-second pending
// poll, which is process-level HTTP (backlog/conversation poller).
//
// The CLI calls these ITSELF, not through a model session: it needs the claim
// token, the resume pointer and the messages BEFORE it can decide whether to
// resume or prime a harness, and it needs them before it spends anything. The
// model only ever calls `reply_conversation`, with the token and session handle
// this client handed the loop.
//
// `claim_conversation` answers `{conversation: null}` when nothing is claimable;
// that is an ordinary empty answer, not an error, and it is the only answer the
// planner has to distinguish from a turn.

// ConversationMessage is one served message. Body is absent on a context
// snippet; Snippet is absent on a full body. The plane caps a served body at
// 4 KB and marks it Truncated rather than cutting silently.
type ConversationMessage struct {
	ID           string `json:"id"`
	Seq          int    `json:"seq"`
	AuthorKind   string `json:"authorKind"`
	AuthorSource string `json:"authorSource"`
	AuthorName   string `json:"authorName"`
	At           string `json:"at"`
	Body         string `json:"body"`
	Snippet      string `json:"snippet"`
	Truncated    bool   `json:"truncated"`
	ReplyToSeq   int    `json:"replyToSeq"`
	QuestionID   string `json:"questionId"`
}

// ConversationResume is the plane's session-affinity pointer: non-null only when
// this instance owns the conversation's stored session. It authorises nothing on
// its own — the machine still needs its own harness session store — so a stale
// ref is an ordinary re-prime, not an error.
type ConversationResume struct {
	Harness string `json:"harness"`
	Ref     string `json:"ref"`
}

// ConversationClaim is one claimed turn: the conversation, the pending messages
// it hands over, the first page of prior context, and the credential that lets
// the session answer it.
type ConversationClaim struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	PendingCount int    `json:"pendingCount"`

	Messages     []ConversationMessage `json:"messages"`
	Context      []ConversationMessage `json:"context"`
	ContextTotal int                   `json:"contextTotal"`
	// Subjects is the union of the pending messages' selected ids, kept raw:
	// its shape belongs to the plane's subject vocabulary and the priming
	// prompt renders it as JSON rather than interpreting it.
	Subjects []json.RawMessage `json:"subjects"`

	Resume         *ConversationResume `json:"resume"`
	LeaseExpiresIn int                 `json:"leaseExpiresIn"`
	ClaimToken     string              `json:"claimToken"`
	Note           string              `json:"note"`
}

// ConversationPage is one page of history, oldest to newest.
type ConversationPage struct {
	Messages      []ConversationMessage `json:"messages"`
	Count         int                   `json:"count"`
	HasMore       bool                  `json:"hasMore"`
	NextBeforeSeq int                   `json:"nextBeforeSeq"`
}

// ConversationClaimOptions are claim_conversation's optional arguments. Every
// one is optional on the wire, so an empty claim is the normal planner path.
type ConversationClaimOptions struct {
	ConversationID string
	// Instance is the CLI's beaconed instance name, for session affinity. The
	// plane refuses a name this key does not beacon under (unknown_instance).
	Instance string
	// History "full" asks for the prior-context page in full, which a fresh
	// session's prime needs; "" leaves the default snippets.
	History string
}

// ConversationAPI is the planner's conversation surface.
type ConversationAPI interface {
	// ClaimConversation claims one turn. A nil claim with a nil error means the
	// plane answered `{conversation: null}` — nothing was claimable, which is a
	// race lost, not a lost message.
	ClaimConversation(ctx context.Context, opts ConversationClaimOptions) (*ConversationClaim, error)
	// ReadConversation pages history strictly older than beforeSeq (0 = the
	// newest page). Read-only: it holds no claim and takes no lease.
	ReadConversation(ctx context.Context, conversationID string, beforeSeq, limit int) (ConversationPage, error)
}

// NewConversationAPI builds the conversation half of the MCP surface. Missing
// either the endpoint or the key yields the not-wired client, degrading exactly
// like New: a planner with no conversation wiring polls nothing.
//
// mcpURL is the project-scoped MCP endpoint (`https://…/mcp/<slug>`).
func NewConversationAPI(mcpURL, apiKey string) ConversationAPI {
	if mcpURL == "" || apiKey == "" {
		return conversationNotWired{}
	}
	return wiredClient(mcpURL, apiKey)
}

// conversationNotWired is NewConversationAPI's not-wired answer — only what
// ConversationAPI names, so it cannot be asserted into a capability nobody
// requested.
type conversationNotWired struct{}

func (conversationNotWired) ClaimConversation(context.Context, ConversationClaimOptions) (*ConversationClaim, error) {
	return nil, ErrNotWired
}

func (conversationNotWired) ReadConversation(context.Context, string, int, int) (ConversationPage, error) {
	return ConversationPage{}, ErrNotWired
}

// ClaimConversation calls claim_conversation once. The absence of a claim is
// returned as (nil, nil): the caller treats it as "next poll", never as an
// error to retry, because a loser of the claim race is offered the next
// conversation by the next poll.
func (r *mcpReleaser) ClaimConversation(ctx context.Context, opts ConversationClaimOptions) (*ConversationClaim, error) {
	args := map[string]any{}
	if opts.ConversationID != "" {
		args["conversationId"] = opts.ConversationID
	}
	if opts.Instance != "" {
		args["instance"] = opts.Instance
	}
	if opts.History != "" {
		args["history"] = opts.History
	}
	raw, err := r.callText(ctx, "claim_conversation", args)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Conversation *struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			PendingCount int    `json:"pendingCount"`
		} `json:"conversation"`
		Messages     []ConversationMessage `json:"messages"`
		Context      []ConversationMessage `json:"context"`
		ContextTotal int                   `json:"contextTotal"`
		Subjects     []json.RawMessage     `json:"subjects"`
		Resume       *ConversationResume   `json:"resume"`
		LeaseExpires int                   `json:"leaseExpiresIn"`
		ClaimToken   string                `json:"claimToken"`
		Note         string                `json:"note"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("claim_conversation: decode result: %w", err)
	}
	if payload.Conversation == nil {
		return nil, nil
	}
	claim := &ConversationClaim{
		ID:             payload.Conversation.ID,
		Title:          payload.Conversation.Title,
		PendingCount:   payload.Conversation.PendingCount,
		Messages:       payload.Messages,
		Context:        payload.Context,
		ContextTotal:   payload.ContextTotal,
		Subjects:       payload.Subjects,
		Resume:         payload.Resume,
		LeaseExpiresIn: payload.LeaseExpires,
		ClaimToken:     payload.ClaimToken,
		Note:           payload.Note,
	}
	if claim.ID == "" || claim.ClaimToken == "" {
		// A claimed turn with no credential cannot be answered; refuse it here
		// rather than spawning a session that can only fail its reply.
		return nil, errors.New("claim_conversation: the plane returned a claim without an id or claimToken")
	}
	return claim, nil
}

// ReadConversation calls read_conversation for one page. beforeSeq <= 0 asks for
// the newest page; limit <= 0 leaves the plane's default (10, capped there).
func (r *mcpReleaser) ReadConversation(ctx context.Context, conversationID string, beforeSeq, limit int) (ConversationPage, error) {
	if conversationID == "" {
		return ConversationPage{}, errors.New("read_conversation: conversationId is required")
	}
	args := map[string]any{"conversationId": conversationID}
	if beforeSeq > 0 {
		args["beforeSeq"] = beforeSeq
	}
	if limit > 0 {
		args["limit"] = limit
	}
	raw, err := r.callText(ctx, "read_conversation", args)
	if err != nil {
		return ConversationPage{}, err
	}
	var page ConversationPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return ConversationPage{}, fmt.Errorf("read_conversation: decode result: %w", err)
	}
	return page, nil
}
