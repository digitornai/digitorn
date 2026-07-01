package toolmw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// dedup collapses identical calls (same tool + params) repeated inside a short
// window into a single execution, returning the cached result for the dupes.
//
// Fix vs the reference, whose cache was a single map shared across every
// session : here the cache is keyed by session, so one session can never serve
// another session's tool result. Only successful results are cached — a
// transient failure must be retried by the next identical call, not memoised.
type dedup struct {
	window     time.Duration
	maxEntries int
	logger     *slog.Logger

	mu       sync.Mutex
	sessions map[string]map[string]dedupEntry
}

type dedupEntry struct {
	res tool.Result
	at  time.Time
}

func newDedup(cfg map[string]any, deps Deps) (Middleware, error) {
	return &dedup{
		window:     secs(cfgFloat(cfg, "window_seconds", 5.0)),
		maxEntries: cfgInt(cfg, "max_entries", 50),
		logger:     deps.Logger,
		sessions:   map[string]map[string]dedupEntry{},
	}, nil
}

func (d *dedup) Name() string { return "dedup" }

func (d *dedup) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	key := callKey(cc.ToolName, cc.Params)
	now := time.Now()

	d.mu.Lock()
	bucket := d.sessions[cc.SessionID]
	if bucket != nil {
		if e, ok := bucket[key]; ok && now.Sub(e.at) < d.window {
			res := e.res
			d.mu.Unlock()
			if d.logger != nil {
				d.logger.Debug("tool_dedup_hit", slog.String("session", cc.SessionID),
					slog.String("module", cc.ModuleID), slog.String("tool", cc.ToolName))
			}
			return res, nil
		}
	}
	d.mu.Unlock()

	res, err := next(ctx, cc)
	if err != nil || !res.Success {
		return res, err
	}

	d.mu.Lock()
	bucket = d.sessions[cc.SessionID]
	if bucket == nil {
		bucket = map[string]dedupEntry{}
		d.sessions[cc.SessionID] = bucket
	}
	bucket[key] = dedupEntry{res: res, at: now}
	if len(bucket) > d.maxEntries {
		evictOldest(bucket)
	}
	d.mu.Unlock()
	return res, nil
}

func callKey(toolName string, params []byte) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0})
	h.Write(params)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func evictOldest(bucket map[string]dedupEntry) {
	var oldestKey string
	var oldest time.Time
	for k, e := range bucket {
		if oldestKey == "" || e.at.Before(oldest) {
			oldestKey, oldest = k, e.at
		}
	}
	delete(bucket, oldestKey)
}
