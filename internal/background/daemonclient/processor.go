package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/runner"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// LaunchSpec is the decoded job payload: what session to feed and with what.
// BG-4's channel pipeline produces these; BG-3 just executes them.
type LaunchSpec struct {
	// AppID overrides the job's AppID when set (a trigger may target another app).
	AppID string `json:"app_id,omitempty"`
	// SessionID, when set, is a SHARED session reused across events (the message
	// is appended). When empty, the processor derives a deterministic per-job id
	// so a lease-expiry retry is idempotent (never a duplicate session/turn).
	SessionID string `json:"session_id,omitempty"`
	// OwnerUserID is the end-user the session belongs to. When set, the client
	// sends X-Act-As-User so the daemon owns the session under that real user
	// (the service must carry the impersonation grant). Empty → the service owns it.
	OwnerUserID string `json:"owner_user_id,omitempty"`
	Message     string `json:"message"`
	Title       string `json:"title,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	Mode        string `json:"mode,omitempty"`
	// EntryAgent pins the app agent that handles the session; Context is extra
	// system-prompt text. Both are honored only on session creation (the daemon
	// stores them in session meta and applies them to every turn).
	EntryAgent string `json:"entry_agent,omitempty"`
	Context    string `json:"context,omitempty"`
	// Model overrides the entry agent's model for the session (set on creation,
	// applied to every turn). Empty → the app's declared default.
	Model string `json:"model,omitempty"`
	// Attachments are inbound media (BlobRefs already uploaded to the daemon) the
	// launching message carries — images/docs the model sees (vision).
	Attachments []BlobRef `json:"attachments,omitempty"`
	// WaitForReply blocks the job until the agent replies (for outbound adapters
	// that must relay the answer). ReplyTimeout bounds the wait.
	WaitForReply bool          `json:"wait_for_reply,omitempty"`
	ReplyTimeout time.Duration `json:"reply_timeout,omitempty"`
	// StreamReply asks the caller to relay the WHOLE agentic turn live (each
	// assistant message + tool activity) via StreamReplies, instead of only the
	// final answer. Launch does not wait when this is set — it returns after the
	// post so the caller can stream from res.UserSeq.
	StreamReply bool `json:"stream_reply,omitempty"`
	// TriggerEvent is the structured inbound event (channels scope) for this
	// launch. Attached to the user message so flow nodes read {{event.payload.*}}.
	TriggerEvent map[string]any `json:"trigger_event,omitempty"`
}

// LaunchResult reports what the invocation did.
type LaunchResult struct {
	SessionID  string
	UserSeq    uint64
	ReplySeq   uint64
	Reply      string
	Created    bool // a new session was created (vs message appended / idempotent skip)
	Idempotent bool // a prior attempt already did the work; this run was a no-op
}

// Launch is the invocation primitive: ensure the session exists (create with the
// inline message, or append to a shared one) and, when requested, wait for the
// agent's reply. Idempotent across retries for the per-job (deterministic id) path.
func (c *Client) Launch(ctx context.Context, spec LaunchSpec, perJobSessionID string) (LaunchResult, error) {
	if spec.Message == "" {
		return LaunchResult{}, errors.New("daemonclient: empty message")
	}
	if spec.AppID == "" {
		return LaunchResult{}, errors.New("daemonclient: empty app id")
	}

	// Own the session under the end-user (impersonation) for every sub-call.
	ctx = withActAs(ctx, spec.OwnerUserID)

	shared := spec.SessionID != ""
	sid := spec.SessionID
	if !shared {
		sid = perJobSessionID
	}

	exists, err := c.SessionExists(ctx, spec.AppID, sid)
	if err != nil {
		return LaunchResult{}, err
	}

	res := LaunchResult{SessionID: sid}
	switch {
	case !exists:
		cr, err := c.CreateSession(ctx, spec.AppID, CreateSessionRequest{
			SessionID:       sid,
			Message:         spec.Message,
			Title:           spec.Title,
			Workdir:         spec.Workdir,
			Mode:            spec.Mode,
			EntryAgent:      spec.EntryAgent,
			Context:         spec.Context,
			Model:           spec.Model,
			Attachments:     spec.Attachments,
			ClientMessageID: perJobSessionID,
			TriggerEvent:    spec.TriggerEvent,
		})
		if err != nil {
			return LaunchResult{}, err
		}
		res.Created = true
		res.UserSeq = cr.FirstMessage.Seq
	case !shared:
		// The session already exists for a per-job (deterministic) id → a prior
		// attempt of THIS job created it and sent the message. Don't re-send.
		res.Idempotent = true
		return res, nil
	default:
		// Shared session that already exists → (re)apply the trigger's model override
		// (the create path sets it inline; an existing session was created without it),
		// then append this event's message so the turn runs on the right model.
		if spec.Model != "" {
			if err := c.SetModel(ctx, spec.AppID, sid, spec.Model); err != nil {
				return LaunchResult{}, err
			}
		}
		pm, err := c.PostMessage(ctx, spec.AppID, sid, PostMessageRequest{
			Message:         spec.Message,
			Mode:            spec.Mode,
			Attachments:     spec.Attachments,
			ClientMessageID: perJobSessionID,
			TriggerEvent:    spec.TriggerEvent,
		})
		if err != nil {
			return LaunchResult{}, err
		}
		res.UserSeq = pm.Seq
	}

	if spec.WaitForReply {
		wctx := ctx
		if spec.ReplyTimeout > 0 {
			var cancel context.CancelFunc
			wctx, cancel = context.WithTimeout(ctx, spec.ReplyTimeout)
			defer cancel()
		}
		msg, err := c.WaitForReply(wctx, spec.AppID, sid, res.UserSeq)
		if err != nil {
			return res, err
		}
		res.Reply = msg.Content
		res.ReplySeq = msg.Seq
	}
	return res, nil
}

// Processor is the runner.Processor that turns a durable job into a daemon
// invocation. It replaces BG-2's logProcessor.
type Processor struct {
	client *Client
	log    *slog.Logger
}

// NewProcessor builds the daemon-invoking processor.
func NewProcessor(client *Client, log *slog.Logger) *Processor {
	if log == nil {
		log = slog.Default()
	}
	return &Processor{client: client, log: log}
}

// Process decodes the job, launches the session, and classifies failures so the
// pool retries transient faults (network / 5xx / 429) and terminally fails
// permanent ones (bad payload / 4xx). The session id is derived from the job id,
// so a retry after a crash never creates a second session.
func (p *Processor) Process(ctx context.Context, job store.Job) error {
	var spec LaunchSpec
	if job.PayloadJSON != "" {
		if err := json.Unmarshal([]byte(job.PayloadJSON), &spec); err != nil {
			return fmt.Errorf("decode payload: %w", err) // terminal: malformed payload
		}
	}
	if spec.AppID == "" {
		spec.AppID = job.AppID
	}
	if spec.Message == "" {
		return errors.New("job has no message to launch") // terminal
	}

	perJob := "bg-" + job.ID
	res, err := p.client.Launch(ctx, spec, perJob)
	if err != nil {
		return Classify(err, job.Attempts)
	}

	p.log.Info("background: launched session",
		"app", spec.AppID, "session", res.SessionID,
		"created", res.Created, "idempotent", res.Idempotent,
		"reply_seq", res.ReplySeq, "dedup", job.DedupKey)
	return nil
}

// Classify maps a daemon error onto the runner's retry contract: transient →
// Retryable with a bounded, attempt-scaled backoff; permanent → terminal.
func Classify(err error, attempts int) error {
	// A reply timeout means the message WAS delivered — never retry (would
	// double-send); surface it terminally for the operator.
	var rt *ErrReplyTimeout
	if errors.As(err, &rt) {
		return err
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Retryable() {
			return runner.Retry(err, backoff(attempts))
		}
		return err
	}
	// Unknown (e.g. our own validation) → terminal.
	return err
}

// backoff is a bounded, attempt-scaled delay (5s, 10s, … capped at 30s).
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Duration(attempts) * 5 * time.Second
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}
