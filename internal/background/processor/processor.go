// Package processor is the composed runner.Processor of the background service:
// it takes a durable job carrying a raw inbound Event, runs the channel
// activation pipeline (BG-4) against the job's trigger config, invokes the
// daemon (BG-3), and — for reply:auto — sends the agent's answer back out on the
// originating adapter. The pipeline runs HERE, after the durable claim, so the
// event is persisted before any processing (the crash-survival contract).
package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
	"github.com/mbathepaul/digitorn/internal/background/channels"
	"github.com/mbathepaul/digitorn/internal/background/daemonclient"
	"github.com/mbathepaul/digitorn/internal/background/runner"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

// TriggerSpec is what an armed trigger persists (as trigger.ConfigJSON): the
// target app + the resolved channel activation + the module-level knobs the
// pipeline needs. The processor loads it per job to run the pipeline.
type TriggerSpec struct {
	AppID        string                    `json:"app_id"`
	Provider     string                    `json:"provider"`
	Adapter      string                    `json:"adapter"`
	DefaultAgent string                    `json:"default_agent,omitempty"`
	SecretFilter bool                      `json:"secret_filter"`
	Activation   channels.ActivationConfig `json:"activation"`
}

func (s TriggerSpec) moduleConfig() channels.ModuleConfig {
	sf := s.SecretFilter
	return channels.ModuleConfig{DefaultAgent: s.DefaultAgent, SecretFilterEnabled: &sf}
}

// maxAttempts bounds retries so a permanently-broken job (bad prepare, dead
// route) eventually fails terminally instead of looping forever.
const maxAttempts = 24

// replyTimeout bounds how long a reply:auto job waits for the agent's answer.
const replyTimeout = 90 * time.Second

// ChannelProcessor implements runner.Processor.
type ChannelProcessor struct {
	store    *store.Store
	client   *daemonclient.Client
	registry *adapter.Registry
	invoker  channels.PrepareInvoker // optional (prepare steps)
	log      *slog.Logger
}

// New builds the processor. registry may be nil if no reply:auto is used;
// invoker may be nil if no trigger uses prepare steps.
func New(st *store.Store, client *daemonclient.Client, registry *adapter.Registry, invoker channels.PrepareInvoker, log *slog.Logger) *ChannelProcessor {
	if log == nil {
		log = slog.Default()
	}
	return &ChannelProcessor{store: st, client: client, registry: registry, invoker: invoker, log: log}
}

// Process runs one durable job end-to-end.
func (p *ChannelProcessor) Process(ctx context.Context, job store.Job) error {
	if job.Attempts > maxAttempts {
		return fmt.Errorf("giving up after %d attempts: %s", job.Attempts, job.LastError)
	}

	var ev adapter.Event
	if err := json.Unmarshal([]byte(job.PayloadJSON), &ev); err != nil {
		return fmt.Errorf("decode event: %w", err) // terminal: malformed
	}
	spec, err := p.loadSpec(ctx, job)
	if err != nil {
		return err
	}

	// Pipeline (BG-4). Payload is sanitized before it reaches templating.
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
		// Pipeline failures (e.g. a prepare module hiccup) are transient: retry
		// with backoff, bounded by maxAttempts above.
		return runner.Retry(err, backoff(job.Attempts))
	}
	if act.Filtered {
		p.log.Info("background: event filtered", "provider", ev.Provider, "reason", act.FilterReason)
		return nil // dropped, durably complete
	}

	// Invoke the daemon (BG-3).
	ls := act.ToLaunchSpec(spec.AppID)
	if ls.WaitForReply {
		ls.ReplyTimeout = replyTimeout
	}
	res, err := p.client.Launch(ctx, ls, "bg-"+job.ID)
	if err != nil {
		return daemonclient.Classify(err, job.Attempts)
	}
	p.log.Info("background: launched", "app", spec.AppID, "session", res.SessionID,
		"agent", act.Agent, "created", res.Created, "idempotent", res.Idempotent)

	// reply:auto → deliver the agent's answer back on the originating adapter.
	if act.Reply == channels.ReplyAuto && res.Reply != "" && len(ev.ReplyRef) > 0 {
		p.sendReply(ctx, ev, spec, res.Reply)
	}
	return nil
}

func (p *ChannelProcessor) sendReply(ctx context.Context, ev adapter.Event, spec TriggerSpec, reply string) {
	if p.registry == nil {
		return
	}
	ad := p.registry.Get(ev.Adapter)
	if ad == nil {
		p.log.Warn("background: no adapter for reply", "adapter", ev.Adapter)
		return
	}
	if spec.SecretFilter {
		reply = channels.FilterSecrets(reply)
	}
	if err := ad.Send(ctx, ev.ReplyRef, reply); err != nil {
		// The session already ran; a failed outbound is logged, not retried (a
		// retry would re-run the whole turn). BG-9 adds a durable send queue.
		p.log.Warn("background: reply send failed", "adapter", ev.Adapter, "err", err.Error())
	}
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
