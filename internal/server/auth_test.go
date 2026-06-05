package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
)

// rsaTestKey is a single in-memory RSA key pair we use across all auth tests.
type rsaTestKey struct {
	priv *rsa.PrivateKey
	kid  string
}

func newRSAKey(t *testing.T, kid string) *rsaTestKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &rsaTestKey{priv: priv, kid: kid}
}

func (k *rsaTestKey) jwksJSON() []byte {
	pub := k.priv.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	// Encode the public exponent as big-endian, stripped of leading zeros.
	eBytes := []byte{byte(pub.E >> 16), byte(pub.E >> 8), byte(pub.E)}
	for len(eBytes) > 1 && eBytes[0] == 0 {
		eBytes = eBytes[1:]
	}
	e := base64.RawURLEncoding.EncodeToString(eBytes)
	doc := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA", "kid": k.kid, "alg": "RS256", "use": "sig",
			"n": n, "e": e,
		}},
	}
	b, _ := json.Marshal(doc)
	return b
}

func (k *rsaTestKey) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// jwksTestServer hosts an OIDC discovery + JWKS endpoint backed by a key.
type jwksTestServer struct {
	server      *httptest.Server
	key         *rsaTestKey
	jwksHits    atomic.Uint64
	oidcHits    atomic.Uint64
	mu          sync.Mutex
	overrideKey *rsaTestKey
}

func newJWKSTestServer(t *testing.T, key *rsaTestKey) *jwksTestServer {
	t.Helper()
	ts := &jwksTestServer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		ts.oidcHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jwks_uri":"%s/.well-known/jwks.json"}`, ts.server.URL)
	})
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		ts.jwksHits.Add(1)
		ts.mu.Lock()
		current := ts.key
		if ts.overrideKey != nil {
			current = ts.overrideKey
		}
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(current.jwksJSON())
	})
	ts.server = httptest.NewServer(mux)
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *jwksTestServer) URL() string { return ts.server.URL }

func startJWKS(t *testing.T, ts *jwksTestServer) *JWKS {
	t.Helper()
	j := NewJWKS(JWKSConfig{
		Issuer:          ts.URL(),
		RefreshInterval: 1 * time.Hour,
		FailureBackoff:  100 * time.Millisecond,
	})
	if err := j.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Stop(context.Background()) })
	return j
}

// ---------- JWKS ----------

func TestJWKS_DiscoveryAndInitialFetch(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	j := startJWKS(t, ts)

	if ts.oidcHits.Load() != 1 {
		t.Fatalf("expected 1 OIDC discovery hit, got %d", ts.oidcHits.Load())
	}
	if ts.jwksHits.Load() != 1 {
		t.Fatalf("expected 1 JWKS hit, got %d", ts.jwksHits.Load())
	}
	if j.KeyCount() != 1 {
		t.Fatalf("expected 1 key, got %d", j.KeyCount())
	}
	pub, err := j.GetKey("key-1")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if pub == nil || pub.N == nil {
		t.Fatal("nil RSA public key")
	}
}

func TestJWKS_MissingKidForcesRefresh(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	j := startJWKS(t, ts)

	// Asking for a kid that's not there should force one refresh.
	initial := ts.jwksHits.Load()
	_, err := j.GetKey("unknown-kid")
	if err == nil {
		t.Fatal("expected ErrJWKSKidMissing")
	}
	if ts.jwksHits.Load() != initial+1 {
		t.Fatalf("expected one force-refresh, got %d (initial %d)", ts.jwksHits.Load(), initial)
	}
}

func TestJWKS_NegativeBackoffOnFailure(t *testing.T) {
	// JWKS server that returns 500 always.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	j := NewJWKS(JWKSConfig{
		Issuer:          srv.URL,
		JWKSURL:         srv.URL + "/.well-known/jwks.json",
		RefreshInterval: 1 * time.Hour,
		FailureBackoff:  200 * time.Millisecond,
	})
	err := j.Start(context.Background())
	if err == nil {
		t.Fatal("expected initial fetch failure")
	}
	if j.failures.Load() == 0 {
		t.Fatal("failure counter not incremented")
	}
}

// ---------- JWT verifier ----------

func makeVerifier(t *testing.T, ts *jwksTestServer, issuer, aud string) *JWTVerifier {
	t.Helper()
	j := startJWKS(t, ts)
	return NewJWTVerifier(j, issuer, aud, "sub", 60*time.Second)
}

func TestJWT_ValidRS256Accepted(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")
	tok := key.sign(t, jwt.MapClaims{
		"sub":   "user-X",
		"iss":   ts.URL(),
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"email": "user@example.com",
		"roles": []string{"admin"},
		"perms": []string{"sessions.read"},
	})
	claims, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != "user-X" {
		t.Fatalf("user_id = %q", claims.UserID)
	}
	if claims.Email != "user@example.com" {
		t.Fatalf("email: %q", claims.Email)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Fatalf("roles: %v", claims.Roles)
	}
	if len(claims.Permissions) != 1 || claims.Permissions[0] != "sessions.read" {
		t.Fatalf("perms: %v", claims.Permissions)
	}
}

func TestJWT_HS256RejectedExplicitly(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "x", "iss": ts.URL(), "exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	signed, _ := tok.SignedString([]byte("secret"))

	_, err := v.Verify(context.Background(), signed)
	if err == nil {
		t.Fatal("HS256 must be rejected")
	}
}

func TestJWT_InvalidSignatureRejected(t *testing.T) {
	keyA := newRSAKey(t, "key-1")
	keyB := newRSAKey(t, "key-1") // same kid, different priv
	ts := newJWKSTestServer(t, keyA)
	v := makeVerifier(t, ts, ts.URL(), "")

	tok := keyB.sign(t, jwt.MapClaims{
		"sub": "x", "iss": ts.URL(), "exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	_, err := v.Verify(context.Background(), tok)
	if err == nil {
		t.Fatal("invalid signature must be rejected")
	}
}

func TestJWT_ExpiredRejected(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")
	tok := key.sign(t, jwt.MapClaims{
		"sub": "x",
		"iss": ts.URL(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	})
	_, err := v.Verify(context.Background(), tok)
	if err == nil {
		t.Fatal("expired must be rejected")
	}
}

func TestJWT_WrongIssuerRejected(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")
	tok := key.sign(t, jwt.MapClaims{
		"sub": "x",
		"iss": "https://wrong.example.com",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	_, err := v.Verify(context.Background(), tok)
	if err == nil {
		t.Fatal("wrong issuer must be rejected")
	}
}

func TestJWT_AudienceCheckEnforced(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "digitorn-api")
	tok := key.sign(t, jwt.MapClaims{
		"sub": "x",
		"iss": ts.URL(),
		"aud": "other-service",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("wrong audience must be rejected")
	}
	// Correct audience accepted.
	tok2 := key.sign(t, jwt.MapClaims{
		"sub": "x",
		"iss": ts.URL(),
		"aud": "digitorn-api",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	if _, err := v.Verify(context.Background(), tok2); err != nil {
		t.Fatal(err)
	}
}

func TestJWT_CustomUserIDClaim(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	j := startJWKS(t, ts)
	v := NewJWTVerifier(j, ts.URL(), "", "user_id", 60*time.Second)
	tok := key.sign(t, jwt.MapClaims{
		"sub":     "ignore-this",
		"user_id": "user-42",
		"iss":     ts.URL(),
		"exp":     time.Now().Add(1 * time.Hour).Unix(),
	})
	c, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID != "user-42" {
		t.Fatalf("user_id from custom claim: %q", c.UserID)
	}
}

// ---------- Auth proxy ----------

func TestAuthProxy_308Redirect(t *testing.T) {
	r := chi.NewRouter()
	MountAuthProxy(r, "https://auth.example.com")
	srv := httptest.NewServer(r)
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/auth/login?next=/foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "https://auth.example.com/auth/login?next=/foo" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestAuthProxy_PreservesAllMethods(t *testing.T) {
	r := chi.NewRouter()
	MountAuthProxy(r, "https://auth.example.com")
	srv := httptest.NewServer(r)
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		req, _ := http.NewRequest(m, srv.URL+"/auth/x", strings.NewReader(""))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusPermanentRedirect {
			t.Fatalf("%s: status = %d", m, resp.StatusCode)
		}
	}
}

// ---------- HTTP middleware ----------

func TestJWTMiddleware_AcceptsBearerWithUserID(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")

	tok := key.sign(t, jwt.MapClaims{
		"sub": "alice", "iss": ts.URL(), "exp": time.Now().Add(time.Hour).Unix(),
	})

	mw := jwtAuthMiddleware(v, false)
	captured := ""
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = userIDOf(r.Context())
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if captured != "alice" {
		t.Fatalf("user_id = %q", captured)
	}
}

func TestJWTMiddleware_RejectsInvalidStrictMode(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")

	mw := jwtAuthMiddleware(v, false)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestJWTMiddleware_DevModeFallbackOnNoToken(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")

	mw := jwtAuthMiddleware(v, true) // dev mode
	captured := ""
	h := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = userIDOf(r.Context())
		w.WriteHeader(200)
	}))
	h = authMiddleware(h)
	h = mw(h)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-User-ID", "dev-bob")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if captured != "dev-bob" {
		t.Fatalf("dev fallback user_id = %q", captured)
	}
}

// ---------- AuthValidator (bridge side) ----------

func TestJWTAuthValidator_AcceptsValidToken(t *testing.T) {
	key := newRSAKey(t, "key-1")
	ts := newJWKSTestServer(t, key)
	v := makeVerifier(t, ts, ts.URL(), "")
	val := &JWTAuthValidator{Verifier: v}

	tok := key.sign(t, jwt.MapClaims{
		"sub": "carol", "iss": ts.URL(), "exp": time.Now().Add(time.Hour).Unix(),
		"perms": []string{"chat"},
	})
	id, err := val.Validate(context.Background(), tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "carol" {
		t.Fatalf("user_id = %q", id.UserID)
	}
	if len(id.Capabilities) != 1 || id.Capabilities[0] != "chat" {
		t.Fatalf("capabilities: %v", id.Capabilities)
	}
}

func TestJWTAuthValidator_DevModeFallbackToMetadataUserID(t *testing.T) {
	val := &JWTAuthValidator{Verifier: nil, DevMode: true}
	id, err := val.Validate(context.Background(), "", map[string]any{"user_id": "dev-dan"})
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "dev-dan" {
		t.Fatalf("user_id = %q", id.UserID)
	}
}

func TestJWTAuthValidator_StrictRejectsNoToken(t *testing.T) {
	val := &JWTAuthValidator{Verifier: nil, DevMode: false}
	_, err := val.Validate(context.Background(), "", nil)
	if err == nil {
		t.Fatal("strict mode must reject empty token")
	}
}
