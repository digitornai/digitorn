package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
	"github.com/digitornai/digitorn/internal/background/channels"
	"github.com/digitornai/digitorn/internal/background/daemonclient"
	"github.com/digitornai/digitorn/internal/background/runner"
	"github.com/digitornai/digitorn/internal/background/store"
)

type TriggerSpec struct {
	AppID        string                    `json:"app_id"`
	Provider     string                    `json:"provider"`
	Adapter      string                    `json:"adapter"`
	DefaultAgent string                    `json:"default_agent,omitempty"`
	SecretFilter bool                      `json:"secret_filter"`
	Activation   channels.ActivationConfig `json:"activation"`
	// Schedule is the cron expression for a cron trigger. Stored in the trigger
	// config so the ops API can report next_run (it is not read by the pipeline).
	Schedule string         `json:"schedule,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
}

func (s TriggerSpec) moduleConfig() channels.ModuleConfig {
	sf := s.SecretFilter
	return channels.ModuleConfig{DefaultAgent: s.DefaultAgent, SecretFilterEnabled: &sf}
}

const maxAttempts = 24

const replyTimeout = 90 * time.Second

const streamTimeout = 300 * time.Second

const approvalPollEvery = 1500 * time.Millisecond

const maxButtonChoices = 25

type ChannelProcessor struct {
	store        *store.Store
	client       *daemonclient.Client
	registry     *adapter.Registry
	invoker      channels.PrepareInvoker
	log          *slog.Logger
	approvalPoll time.Duration
}

func New(st *store.Store, client *daemonclient.Client, registry *adapter.Registry, invoker channels.PrepareInvoker, log *slog.Logger) *ChannelProcessor {
	if log == nil {
		log = slog.Default()
	}
	return &ChannelProcessor{store: st, client: client, registry: registry, invoker: invoker, log: log, approvalPoll: approvalPollEvery}
}

func (p *ChannelProcessor) Process(ctx context.Context, job store.Job) (err error) {
	started := time.Now()
	rr := runInfo{jobID: job.ID, appID: job.AppID, triggerID: job.TriggerID, provider: job.Provider, attempt: job.Attempts}
	defer func() { p.recordRun(rr, started, err) }()

	if job.Attempts > maxAttempts {
		return fmt.Errorf("giving up after %d attempts: %s", job.Attempts, job.LastError)
	}

	var ev adapter.Event
	if e := json.Unmarshal([]byte(job.PayloadJSON), &ev); e != nil {
		return fmt.Errorf("decode event: %w", e)
	}
	spec, err := p.loadSpec(ctx, job)
	if err != nil {
		return err
	}
	rr.provider, rr.adapter = ev.Provider, ev.Adapter

	cev := channels.Event{
		EventID:  job.ID,
		Provider: ev.Provider,
		Adapter:  ev.Adapter,
		Source:   ev.Source,
		Message:  ev.Message,
		Payload:  channels.SanitizePayload(ev.Payload),
		Metadata: ev.Metadata,
	}
	act, err := channels.Process(ctx, cev, spec.Activation, spec.moduleConfig(), p.invoker)
	if err != nil {
		return runner.Retry(err, backoff(job.Attempts))
	}
	if act.Filtered {
		rr.filtered = true
		p.log.Info("background: event filtered", "provider", ev.Provider, "reason", act.FilterReason)
		return nil
	}

	if act.Deliver != nil && act.Reply != channels.ReplyAuto {
		rr.pushed = true
		rr.replyChars, rr.replyPreview = len(act.Message), runPreview(act.Message)
		p.deliverReply(ctx, ev, act, spec, act.Message)
		p.log.Info("background: raw push delivered", "provider", ev.Provider, "to", act.Deliver.Adapter)
		return nil
	}

	ls := act.ToLaunchSpec(spec.AppID)
	if len(ev.Attachments) > 0 {
		ls.Attachments = p.resolveAttachments(ctx, ev, ls.AppID)
	}
	ls.Attachments = append(ls.Attachments, inputAttachments(spec.Activation.Attachments)...)
	if spec.Activation.Reports {
		ls.Message = withReportFolder(ls.Message, time.Now())
	}
	if ls.WaitForReply || ls.StreamReply {
		ls.ReplyTimeout = replyTimeout
		if stop := p.keepTyping(ctx, ev); stop != nil {
			defer stop()
		}
		sid := ls.SessionID
		if sid == "" {
			sid = "bg-" + job.ID
		}
		if stop := p.pumpApprovals(daemonclient.WithActAs(ctx, ls.OwnerUserID), ev, ls.AppID, sid); stop != nil {
			defer stop()
		}
	}
	res, err := p.client.Launch(ctx, ls, "bg-"+job.ID)
	if err != nil {
		return daemonclient.Classify(err, job.Attempts)
	}
	rr.sessionID = res.SessionID
	p.log.Info("background: launched", "app", spec.AppID, "session", res.SessionID,
		"agent", act.Agent, "created", res.Created, "idempotent", res.Idempotent)

	switch {
	case ls.StreamReply && !res.Idempotent:
		// Surface the WHOLE agentic loop in the channel: each assistant message + a
		// compact line per tool call, live, until the turn settles.
		p.log.Info("background: streaming turn", "session", res.SessionID, "after_seq", res.UserSeq, "adapter", ev.Adapter)
		sctx, cancel := context.WithTimeout(daemonclient.WithActAs(ctx, ls.OwnerUserID), streamTimeout)
		defer cancel()
		n := 0
		serr := p.client.StreamReplies(sctx, ls.AppID, res.SessionID, res.UserSeq, func(item daemonclient.StreamItem) {
			n++
			rr.replyChars += len(item.Text)
			if t := strings.TrimSpace(item.Text); t != "" {
				rr.replyPreview = runPreview(t) // the latest message is the report excerpt
			}
			p.deliverReply(ctx, ev, act, spec, item.Text)
		})
		p.log.Info("background: stream done", "session", res.SessionID, "items", n, "err", errStr(serr))
	case res.Reply != "":
		// Final answer only (reply:auto), to the push destination or the originator.
		rr.replyChars, rr.replyPreview = len(res.Reply), runPreview(res.Reply)
		p.deliverReply(ctx, ev, act, spec, res.Reply)
	case !res.Idempotent:
		octx, cancel := context.WithTimeout(daemonclient.WithActAs(ctx, ls.OwnerUserID), replyTimeout)
		if turnErr, aerr := p.client.AwaitTurnOutcome(octx, ls.AppID, res.SessionID, res.UserSeq); aerr == nil && turnErr != "" {
			rr.turnErr = errors.New(turnErr)
		}
		cancel()
	}
	return nil
}

// runInfo accumulates the execution-report fields across Process's branches; the
// deferred recordRun turns it into one durable bg_runs row.
type runInfo struct {
	jobID, appID, triggerID, provider, adapter, sessionID, replyPreview string
	attempt, replyChars                                                 int
	filtered, pushed                                                    bool
	turnErr                                                             error
}

// recordRun writes the execution report for one attempt. Outcome is derived from
// the terminal error + the path taken. Best-effort + detached ctx so a cancelled
// or failed job still leaves a report; a write failure is logged, never fatal.
func (p *ChannelProcessor) recordRun(rr runInfo, started time.Time, err error) {
	if p.store == nil {
		return
	}
	outcome, errStr := "ok", ""
	switch {
	case err != nil:
		errStr = clipStr(err.Error(), 2000)
		var rt *runner.Retryable
		if errors.As(err, &rt) {
			outcome = "retrying"
		} else {
			outcome = "failed"
		}
	case rr.turnErr != nil:
		outcome = "failed"
		errStr = clipStr(rr.turnErr.Error(), 2000)
	case rr.filtered:
		outcome = "filtered"
	case rr.pushed:
		outcome = "pushed"
	}
	rec := store.Run{
		JobID: rr.jobID, AppID: rr.appID, TriggerID: rr.triggerID,
		Provider: rr.provider, Adapter: rr.adapter, Attempt: rr.attempt,
		Outcome: outcome, SessionID: rr.sessionID,
		ReplyChars: rr.replyChars, ReplyPreview: rr.replyPreview, Error: errStr,
		StartedAt: started.UTC(), EndedAt: time.Now().UTC(),
		DurationMs: time.Since(started).Milliseconds(),
	}
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := p.store.RecordRun(rctx, rec); e != nil {
		p.log.Warn("background: record run failed", "job", rr.jobID, "err", e.Error())
	}
}

// runPreview is a single-line, rune-safe, capped excerpt of a reply for the report.
func runPreview(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 280 {
		return string(r[:280]) + "…"
	}
	return s
}

// inputAttachments converts the schedule-carried input blob refs to the daemon
// wire shape, skipping empty entries. Forwarded verbatim — the blobs already
// live in the app store, so each fire re-attaches the same ref (no re-upload).
func inputAttachments(refs []channels.AttachmentRef) []daemonclient.BlobRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]daemonclient.BlobRef, 0, len(refs))
	for _, a := range refs {
		if a.Hash != "" {
			out = append(out, daemonclient.BlobRef{Hash: a.Hash, Mime: a.Mime, Size: a.Size})
		}
	}
	return out
}

// reportDirStamp names the per-fire output subfolder : sortable, unambiguous,
// one per run. UTC so a session woken across time zones stays consistent.
func reportDirStamp(t time.Time) string { return t.UTC().Format("2006-01-02_150405") }

// withReportFolder appends, to the per-fire message, an instruction giving the
// agent a dated, workdir-relative folder to write its outputs into. It rides the
// MESSAGE (not Context, which is create-only) so it reaches the agent on every
// fire of a persistent session. The agent writes via its own workdir-confined
// tools; the folder is then listable/downloadable via the /workspace routes.
func withReportFolder(msg string, t time.Time) string {
	rel := "attachments/" + reportDirStamp(t)
	return msg + "\n\n[Report folder] Save every file, report, or document you produce in this run under `" +
		rel + "/` (create the directory first, relative to your workspace). Files placed there are preserved and downloadable by the user."
}

// clipStr caps a string to n runes (for the stored error column).
func clipStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// deliverReply sends text to the right channel: the activation's configured push
// Destination when set (proactive — a cron/webhook with no inbound channel), else
// the inbound event's reply handle (reactive — answer the originator). One path for
// both, so every adapter with Send is a push target with zero extra code.
func (p *ChannelProcessor) deliverReply(ctx context.Context, ev adapter.Event, act channels.Activation, spec TriggerSpec, text string) {
	if p.registry == nil || strings.TrimSpace(text) == "" {
		return
	}
	adapterName, ref := ev.Adapter, ev.ReplyRef
	if act.Deliver != nil {
		adapterName, ref = act.Deliver.Adapter, act.Deliver.Ref
	}
	if adapterName == "" || len(ref) == 0 {
		return
	}
	ad := p.registry.Get(adapterName)
	if ad == nil {
		p.log.Warn("background: no adapter for delivery", "adapter", adapterName)
		return
	}
	if spec.SecretFilter {
		text = channels.FilterSecrets(text)
	}
	if err := ad.Send(ctx, ref, text); err != nil {
		// The session already ran; a failed outbound is logged, not retried (a
		// retry would re-run the whole turn). BG-9 adds a durable send queue.
		p.log.Warn("background: delivery send failed", "adapter", adapterName, "err", err.Error())
		return
	}
	p.log.Info("background: delivered", "adapter", adapterName, "chars", len(text))
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// resolveAttachments downloads each inbound attachment via the adapter's MediaFetcher
// and uploads it to the daemon's blob store, returning the BlobRefs the launching
// message carries. Generic over any MediaFetcher adapter; best-effort per file (a
// failed fetch/upload is logged + skipped, never aborting the turn).
func (p *ChannelProcessor) resolveAttachments(ctx context.Context, ev adapter.Event, appID string) []daemonclient.BlobRef {
	if p.registry == nil {
		return nil
	}
	fetcher, ok := p.registry.Get(ev.Adapter).(adapter.MediaFetcher)
	if !ok {
		return nil
	}
	out := make([]daemonclient.BlobRef, 0, len(ev.Attachments))
	for _, att := range ev.Attachments {
		data, mime, err := fetcher.FetchMedia(ctx, att)
		if err != nil {
			p.log.Warn("background: fetch media failed", "provider", ev.Provider, "file", att.Filename, "err", err.Error())
			continue
		}
		if len(data) == 0 {
			continue
		}
		if mime == "" {
			mime = att.ContentType
		}
		ref, err := p.client.UploadBlob(ctx, appID, mime, data)
		if err != nil {
			p.log.Warn("background: upload media failed", "provider", ev.Provider, "err", err.Error())
			continue
		}
		out = append(out, ref)
	}
	return out
}

// keepTyping drives the originating adapter's OPTIONAL presence indicator until the
// returned stop() is called (deferred at the end of the turn). Generic: it works for
// ANY adapter implementing adapter.Typer (Discord, Telegram, …) and is a no-op for
// those that can't express presence — zero per-channel code in the processor.
func (p *ChannelProcessor) keepTyping(ctx context.Context, ev adapter.Event) func() {
	if p.registry == nil || len(ev.ReplyRef) == 0 {
		return nil
	}
	typer, ok := p.registry.Get(ev.Adapter).(adapter.Typer)
	if !ok {
		return nil
	}
	stop := make(chan struct{})
	go func() {
		// Most channels' typing hints last ~5–10 s; refresh just under that.
		t := time.NewTicker(6 * time.Second)
		defer t.Stop()
		_ = typer.Typing(ctx, ev.ReplyRef) // immediate, before the first tick
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = typer.Typing(ctx, ev.ReplyRef)
			}
		}
	}()
	return func() { close(stop) }
}

// pumpApprovals drives the originating adapter's OPTIONAL human-in-the-loop control
// (buttons / modal) for the duration of the turn. While the turn runs it polls the
// session for parked approvals (gated tools, ask_user questions); each new one is
// handed — concurrently, so parallel tool approvals surface at once — to the adapter's
// Prompter, and the user's answer is resolved back to the daemon. Generic: it works
// for ANY adapter implementing adapter.Prompter and is a no-op for the rest (the
// approval stays resolvable via web/CLI or times out) — zero per-channel code here.
func (p *ChannelProcessor) pumpApprovals(ctx context.Context, ev adapter.Event, appID, sessionID string) func() {
	if p.registry == nil || len(ev.ReplyRef) == 0 {
		return nil
	}
	prompter, ok := p.registry.Get(ev.Adapter).(adapter.Prompter)
	if !ok {
		return nil
	}
	pctx, cancel := context.WithCancel(ctx)
	var (
		mu      sync.Mutex
		handled = map[string]bool{}
		wg      sync.WaitGroup
	)
	pollEvery := p.approvalPoll
	if pollEvery <= 0 {
		pollEvery = approvalPollEvery
	}
	go func() {
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
			}
			aps, err := p.client.PendingApprovals(pctx, appID, sessionID)
			if err != nil {
				continue // session not created yet / transient — keep polling
			}
			for _, ap := range aps {
				if ap.ApprovalID == "" {
					continue
				}
				mu.Lock()
				seen := handled[ap.ApprovalID]
				if !seen {
					handled[ap.ApprovalID] = true
				}
				mu.Unlock()
				if seen {
					continue
				}
				wg.Add(1)
				go func(ap daemonclient.Approval) {
					defer wg.Done()
					p.handleApproval(pctx, prompter, ev.ReplyRef, appID, sessionID, ap)
				}(ap)
			}
		}
	}()
	return func() { cancel(); wg.Wait() }
}

// handleApproval renders ONE parked decision in the channel and resolves the answer.
// A render that the channel can't express (multi-select, form) or a prompt error
// leaves the approval unresolved — it stays answerable via web/CLI, or times out.
func (p *ChannelProcessor) handleApproval(ctx context.Context, prompter adapter.Prompter, replyRef map[string]any, appID, sessionID string, ap daemonclient.Approval) {
	req, ok := buildPromptRequest(replyRef, ap)
	if !ok {
		p.log.Info("background: approval not expressible in channel, deferring to web/CLI",
			"kind", ap.Kind, "approval", ap.ApprovalID)
		return
	}
	resp, err := prompter.Prompt(ctx, req)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("background: approval prompt failed", "approval", ap.ApprovalID, "err", err.Error())
		}
		return
	}
	action, reason := mapPromptResponse(ap, resp)
	if err := p.client.ResolveApproval(ctx, appID, sessionID, ap.ApprovalID, action, reason); err != nil {
		p.log.Warn("background: resolve approval failed", "approval", ap.ApprovalID, "err", err.Error())
		return
	}
	p.log.Info("background: approval resolved in channel",
		"kind", ap.Kind, "approval", ap.ApprovalID, "action", action, "by", resp.UserID)
}

// buildPromptRequest maps a parked approval onto the generic Prompter request. It
// returns ok=false for shapes a button/modal channel can't faithfully express
// (multi-select, structured form) so the caller can degrade instead of mis-rendering.
func buildPromptRequest(replyRef map[string]any, ap daemonclient.Approval) (adapter.PromptRequest, bool) {
	switch ap.Kind {
	case "question":
		if truthy(ap.Payload["allow_multiple"]) || hasForm(ap.Payload) {
			return adapter.PromptRequest{}, false
		}
		choices := stringSlice(ap.Payload["choices"])
		if len(choices) > maxButtonChoices {
			return adapter.PromptRequest{}, false
		}
		req := adapter.PromptRequest{
			ReplyRef: replyRef,
			Title:    "❓ Question",
			Body:     questionBody(ap),
		}
		for _, ch := range choices {
			req.Options = append(req.Options, adapter.PromptOption{ID: ch, Label: truncate(ch, 80), Style: "secondary"})
		}
		// Free text when there are no choices, or the agent allows a custom answer.
		if len(choices) == 0 || truthy(ap.Payload["allow_custom"]) {
			req.AllowText = true
			req.TextLabel = "Votre réponse"
			req.TextPlaceholder = strVal(ap.Payload["placeholder"])
			req.Multiline = truthy(ap.Payload["multiline"])
		}
		return req, true
	default: // tool_call (and any future approve-shaped kind)
		return adapter.PromptRequest{
			ReplyRef: replyRef,
			Title:    "🔒 Approbation requise",
			Body:     toolBody(ap),
			Options: []adapter.PromptOption{
				{ID: "grant", Label: "✅ Approuver", Style: "primary"},
				{ID: "deny", Label: "❌ Refuser", Style: "danger"},
			},
		}, true
	}
}

// mapPromptResponse turns the user's answer into a daemon resolve (action, reason).
// Free text and a chosen choice both resolve as "grant" (the ask_user answer rides
// in reason); a tool_call's option id IS the action ("grant"/"deny").
func mapPromptResponse(ap daemonclient.Approval, resp adapter.PromptResponse) (action, reason string) {
	if strings.TrimSpace(resp.Text) != "" {
		return "grant", resp.Text
	}
	if ap.Kind == "question" {
		return "grant", resp.OptionID
	}
	action = resp.OptionID
	if action == "" {
		action = "deny"
	}
	reason = "via channel"
	if resp.UserID != "" {
		reason = "via channel (user " + resp.UserID + ")"
	}
	return action, reason
}

// toolBody renders a gated tool's params for the channel prompt — readable, not a raw
// escaped-JSON dump: the tool + risk + reason, then the file's basename (the full
// workdir path is noise) and its content as a real, language-highlighted code block
// (truncated). Non-file tools fall back to compact "key: value" lines.
func toolBody(ap daemonclient.Approval) string {
	var b strings.Builder
	if ap.ToolName != "" {
		fmt.Fprintf(&b, "**Outil :** `%s`", ap.ToolName)
	}
	if ap.RiskLevel != "" {
		fmt.Fprintf(&b, "  · risque **%s**", ap.RiskLevel)
	}
	if ap.Reason != "" {
		b.WriteByte('\n')
		b.WriteString(ap.Reason)
	}
	p := ap.ToolParams
	if path := strVal(p["path"]); path != "" {
		fmt.Fprintf(&b, "\n📄 `%s`", baseName(path))
	}
	if code, lang := codeParam(p); code != "" {
		fmt.Fprintf(&b, "\n```%s\n%s\n```", lang, clipCode(code, 24, 1400))
	} else if kv := scalarParams(p); kv != "" {
		fmt.Fprintf(&b, "\n%s", kv)
	}
	if b.Len() == 0 {
		return "L'agent demande l'autorisation d'exécuter une action."
	}
	return sanitizeForChannel(b.String())
}

// sanitizeForChannel makes a string safe to post to a chat channel: it repairs
// invalid UTF-8 (a model's tool params can carry raw/binary bytes) into the
// replacement rune, and drops non-printable control characters except newline and
// tab. Without this, a tool param with binary content would yield a message the
// channel API rejects or mangles.
func sanitizeForChannel(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// baseName returns the last path segment, handling both / and \ regardless of OS.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// codeParam extracts a tool's main text body (file content / shell command / text)
// and a language hint for syntax highlighting.
func codeParam(p map[string]any) (string, string) {
	if c := strVal(p["content"]); c != "" {
		return c, langFromPath(strVal(p["path"]))
	}
	if c := strVal(p["command"]); c != "" {
		return c, "bash"
	}
	if c := strVal(p["text"]); c != "" {
		return c, ""
	}
	return "", ""
}

func langFromPath(path string) string {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return ""
	}
	switch strings.ToLower(path[dot+1:]) {
	case "py":
		return "python"
	case "go":
		return "go"
	case "js", "mjs", "cjs":
		return "javascript"
	case "ts", "tsx":
		return "typescript"
	case "json":
		return "json"
	case "yaml", "yml":
		return "yaml"
	case "sh", "bash":
		return "bash"
	case "md":
		return "markdown"
	case "html", "htm":
		return "html"
	case "css":
		return "css"
	case "sql":
		return "sql"
	case "rs":
		return "rust"
	case "java":
		return "java"
	case "rb":
		return "ruby"
	case "php":
		return "php"
	case "toml":
		return "toml"
	default:
		return ""
	}
}

// clipCode caps a code body to maxLines / maxChars, noting how many lines were hidden.
func clipCode(s string, maxLines, maxChars int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	hidden := 0
	if len(lines) > maxLines {
		hidden = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	out := strings.Join(lines, "\n")
	if len([]rune(out)) > maxChars {
		out = truncate(out, maxChars)
	}
	if hidden > 0 {
		out += fmt.Sprintf("\n… (+%d lignes)", hidden)
	}
	return out
}

// scalarParams renders the params OTHER than the main body (path/content/command/text)
// as compact, sorted "• key: value" lines.
func scalarParams(p map[string]any) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		switch k {
		case "path", "content", "command", "text":
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		var vs string
		if s, ok := p[k].(string); ok {
			vs = s
		} else {
			raw, _ := json.Marshal(p[k])
			vs = string(raw)
		}
		fmt.Fprintf(&b, "• %s: %s\n", k, truncate(vs, 120))
	}
	return strings.TrimRight(b.String(), "\n")
}

func questionBody(ap daemonclient.Approval) string {
	body := ap.Reason
	if c := strVal(ap.Payload["content"]); c != "" && c != body {
		if body != "" {
			body += "\n\n"
		}
		body += c
	}
	if body == "" {
		body = "L'agent te pose une question."
	}
	return body
}

func truthy(v any) bool { b, _ := v.(bool); return b }

func hasForm(p map[string]any) bool {
	if p == nil {
		return false
	}
	switch f := p["form"].(type) {
	case []any:
		return len(f) > 0
	case map[string]any:
		return len(f) > 0
	}
	return false
}

func strVal(v any) string { s, _ := v.(string); return s }

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// loadSpec reads the job's trigger config. A job with no trigger, or whose
// trigger/config is unreadable, is terminal (it can never succeed).
func (p *ChannelProcessor) loadSpec(ctx context.Context, job store.Job) (TriggerSpec, error) {
	if job.TriggerID == "" {
		return TriggerSpec{}, errors.New("job has no trigger")
	}
	tr, err := p.store.GetTrigger(ctx, job.TriggerID)
	if err != nil {
		return TriggerSpec{}, fmt.Errorf("load trigger %q: %w", job.TriggerID, err)
	}
	var spec TriggerSpec
	if err := json.Unmarshal([]byte(tr.ConfigJSON), &spec); err != nil {
		return TriggerSpec{}, fmt.Errorf("decode trigger config: %w", err)
	}
	if spec.AppID == "" {
		spec.AppID = job.AppID
	}
	return spec, nil
}

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
