package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// appendWaitBudget caps how long a REST handler waits for queue space
// before returning 503 to the client. Short enough that clients see
// fast backpressure ; long enough that healthy bursts (a few hundred
// ms of disk-fsync catch-up) drain without surfacing as errors.
const appendWaitBudget = 2 * time.Second

// appendCtx returns a child context bounded by appendWaitBudget so that
// AppendBlocking cannot stall a handler indefinitely under saturation.
// If the parent context already has a tighter deadline, that one wins.
func appendCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, appendWaitBudget)
}

// appendErrStatus maps an AppendDurable / AppendBlocking error to an HTTP status :
//   - ctx.DeadlineExceeded / Canceled → 503 (Service Unavailable, retry)
//   - ErrQueueFull (per-sid quota tripped) → 429 (Too Many Requests)
//   - ErrBusStopped → 503 (daemon shutting down)
//   - other → 500
func appendErrStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, sessionstore.ErrQueueFull) {
		return http.StatusTooManyRequests
	}
	if errors.Is(err, sessionstore.ErrBusStopped) || errors.Is(err, sessionstore.ErrFlusherStop) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

type userKey struct{}

// withUserID attaches a user_id to a request context.
func withUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userKey{}, userID)
}

// userIDOf extracts the resolved user_id from a request context.
func userIDOf(ctx context.Context) string {
	v, _ := ctx.Value(userKey{}).(string)
	return v
}

// authMiddleware resolves a user_id for the request. Dev-mode fallback
// (X-User-ID header) is kept for local development when no auth service
// is configured. Real JWT validation is layered on by jwtAuthMiddleware
// when the Auth.Enabled config flag is set.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If the JWT middleware already set a user_id, skip.
		if userIDOf(r.Context()) != "" {
			next.ServeHTTP(w, r)
			return
		}
		uid := strings.TrimSpace(r.Header.Get("X-User-ID"))
		if uid == "" {
			uid = strings.TrimSpace(r.URL.Query().Get("user_id"))
		}
		if uid == "" {
			uid = "anonymous"
		}
		r = r.WithContext(withUserID(r.Context(), uid))
		next.ServeHTTP(w, r)
	})
}

// jwtAuthMiddleware extracts a Bearer token, validates it via the JWKS
// keyset, and injects the resolved user_id (+ roles + permissions) into
// the request context. On invalid / missing token : 401 in strict mode,
// pass-through to the next middleware in dev mode (which falls back to
// X-User-ID).
func jwtAuthMiddleware(verifier *JWTVerifier, devMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if verifier == nil {
				next.ServeHTTP(w, r)
				return
			}
			tok := extractBearer(r)
			if tok == "" {
				if devMode {
					next.ServeHTTP(w, r)
					return
				}
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			claims, err := verifier.Verify(r.Context(), tok)
			if err != nil {
				if devMode {
					next.ServeHTTP(w, r)
					return
				}
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			ctx := withUserID(r.Context(), claims.UserID)
			ctx = withClaims(ctx, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

type claimsKey struct{}

func withClaims(ctx context.Context, c *VerifierClaims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// ClaimsOf returns the JWT claims attached to the context (nil if dev mode
// or no JWT was attached).
func ClaimsOf(ctx context.Context) *VerifierClaims {
	v, _ := ctx.Value(claimsKey{}).(*VerifierClaims)
	return v
}

// ── On-behalf-of impersonation ───────────────────────────────────────────────
//
// A trusted service (its JWT carrying permImpersonate or roleService) may create /
// act on sessions on behalf of an end-user by presenting the X-Act-As-User header.
// The daemon then treats the EFFECTIVE user as that end-user, so ALL existing
// per-user isolation (ownership, listing, workdir, quota) applies to the real user
// automatically — while the actor (the service) is recorded for audit. This is how
// the background service launches per-end-user sessions without a per-user token.

const (
	permImpersonate = "sessions:impersonate"
	roleService     = "service"
	actAsHeader     = "X-Act-As-User"
)

type actorKey struct{}

func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// actorOf returns the real caller identity when the request impersonates another
// user ("" otherwise).
func actorOf(ctx context.Context) string {
	v, _ := ctx.Value(actorKey{}).(string)
	return v
}

// callerCanImpersonate reports whether the authenticated caller may act on behalf
// of another user : it must carry the impersonation permission or the service role.
func callerCanImpersonate(c *VerifierClaims) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Permissions {
		if p == permImpersonate {
			return true
		}
	}
	for _, r := range c.Roles {
		if r == roleService {
			return true
		}
	}
	return false
}

// actAsMiddleware resolves trusted impersonation. When X-Act-As-User is present :
//   - auth ON  : the caller MUST carry the impersonation grant, else 403.
//   - auth OFF (dev) : honored unconditionally (no real identity to protect).
//
// On success the effective ctx user becomes the end-user and the original caller is
// stashed as the actor. Absent header → untouched (byte-identical to before).
func (d *Daemon) actAsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actAs := strings.TrimSpace(r.Header.Get(actAsHeader))
		if actAs == "" {
			next.ServeHTTP(w, r)
			return
		}
		authOn := d.cfg != nil && d.cfg.Auth.Enabled
		if authOn && !callerCanImpersonate(ClaimsOf(r.Context())) {
			writeError(w, http.StatusForbidden, "forbidden", "impersonation not permitted for this caller")
			return
		}
		ctx := withActor(r.Context(), userIDOf(r.Context()))
		ctx = withUserID(ctx, actAs)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": message,
	})
}

func readJSON(r *http.Request, out any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// readJSONLenient decodes a body without rejecting unknown fields. Used by the
// client-facing write endpoints (session create, message post) that browsers
// call with a richer payload than the daemon consumes — queue hints, a dedup
// id, attachments not yet wired. Those extras are ignored instead of failing
// the whole request; the strict readJSON stays the default everywhere else.
func readJSONLenient(r *http.Request, out any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func notImplemented(w http.ResponseWriter, feature string) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":   "not_implemented",
		"feature": feature,
		"message": "this endpoint is registered for client compatibility but requires the runtime; it will be implemented in a later iteration",
	})
}

func parseIntQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseUint64Query(r *http.Request, key string, def uint64) uint64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
