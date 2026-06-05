// Package worker provides a generic subprocess worker framework used by
// digitornd to host modules that need crash isolation or independent
// scaling : LLM, shell, browser, OCR, … LLM is the first consumer ;
// the framework itself is module-agnostic.
package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"google.golang.org/grpc"
)

// Kind identifies a worker class (one binary, one role). Examples : "llm",
// "shell", "browser". All instances of the same Kind run the same binary
// and the same service.
type Kind string

// Status is the lifecycle phase of one worker instance.
type Status int32

const (
	StatusStarting Status = iota
	StatusReady
	StatusRunning
	StatusDraining
	StatusStopped
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusStarting:
		return "starting"
	case StatusReady:
		return "ready"
	case StatusRunning:
		return "running"
	case StatusDraining:
		return "draining"
	case StatusStopped:
		return "stopped"
	case StatusFailed:
		return "failed"
	}
	return "unknown"
}

// Spec describes how to spawn a single Kind of worker. Count instances
// will be spawned ; each runs in its own subprocess with its own port.
type Spec struct {
	Kind         Kind
	Binary       string            // path to the worker binary
	Args         []string          // command-line args (the shared secret is passed via env, not args)
	Env          map[string]string // extra env vars (the secret is added automatically)
	Count        int               // number of instances to spawn (>=1)
	StartTimeout time.Duration     // how long to wait for the first Health=SERVING (default 10s)
	StopTimeout  time.Duration     // how long to wait for graceful stop after SIGTERM (default 10s)
	HealthEvery  time.Duration     // periodic health-check interval (default 5s)
	BackoffMin   time.Duration     // restart backoff start (default 500ms)
	BackoffMax   time.Duration     // restart backoff cap (default 30s)
	MaxFailures  int               // consecutive failures before giving up (default 5; 0=unlimited)
	StdoutRing   int               // ring buffer line count for diagnostics (default 256)
}

// applyDefaults fills zero-value fields with sensible defaults.
func (s *Spec) applyDefaults() {
	if s.Count <= 0 {
		s.Count = 1
	}
	if s.StartTimeout <= 0 {
		s.StartTimeout = 10 * time.Second
	}
	if s.StopTimeout <= 0 {
		s.StopTimeout = 10 * time.Second
	}
	if s.HealthEvery <= 0 {
		s.HealthEvery = 5 * time.Second
	}
	if s.BackoffMin <= 0 {
		s.BackoffMin = 500 * time.Millisecond
	}
	if s.BackoffMax <= 0 {
		s.BackoffMax = 30 * time.Second
	}
	if s.MaxFailures < 0 {
		s.MaxFailures = 0
	}
	if s.StdoutRing <= 0 {
		s.StdoutRing = 256
	}
}

// Handle is a read-only snapshot of one worker instance, returned by
// Manager.Pool() so the caller can route requests.
type Handle struct {
	ID       string // generated per-instance ID (kind#index)
	Kind     Kind
	PID      int
	Address  string // gRPC address (e.g. "127.0.0.1:34917")
	Status   Status
	Restarts int
	StartAt  time.Time
}

// Conn returns a gRPC client connection to this worker. Connection is
// re-established on demand. Caller does NOT close the returned conn —
// the framework owns it.
type Conn interface {
	GRPC() *grpc.ClientConn
	Handle() Handle
	Close() error
}

// Generic errors.
var (
	ErrNoHealthyWorker = errors.New("worker: no healthy instance available")
	ErrManagerStopped  = errors.New("worker: manager stopped")
	ErrInvalidSpec     = errors.New("worker: invalid spec")
	ErrSpawnFailed     = errors.New("worker: spawn failed")
	ErrStartupTimeout  = errors.New("worker: startup timeout")
	ErrTooManyFailures = errors.New("worker: max consecutive failures exceeded")
	ErrAuthBadSecret   = errors.New("worker: handshake secret mismatch")
)

// generateSecret returns a 32-byte hex-encoded random secret. Master
// generates one per worker process and passes it via env var to the
// child. The child injects it into every gRPC call (header) ; the master
// expects the same value back so a foreign process cannot trivially
// connect to a worker's gRPC port.
func generateSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// EnvSecretKey is the name of the env var workers read at startup to learn
// the shared secret. Master sets it ; worker reads it and feeds it to the
// gRPC interceptors.
const EnvSecretKey = "DIGITORN_WORKER_SECRET"

// EnvBindKey is the env var a worker reads to learn what address to bind.
// "127.0.0.1:0" (default) binds TCP loopback on an OS-picked port ; a
// "unix:<path>" value binds an AF_UNIX socket at <path> — same-host IPC
// with ~2.8× lower round-trip latency than TCP loopback. The bound address
// is published on stdout for the master to dial either way.
const EnvBindKey = "DIGITORN_WORKER_BIND"

// BindAddrFromEnv returns the worker bind address from EnvBindKey, falling
// back to TCP loopback on an OS-picked port. Worker binaries pass the
// result to ServerConfig.BindAddr.
func BindAddrFromEnv() string {
	return envOr(EnvBindKey, "127.0.0.1:0")
}

// HeaderSecret is the gRPC metadata key carrying the secret.
const HeaderSecret = "x-digitorn-worker-secret"

// HeaderWorkerKind identifies the worker kind on the wire (for logs).
const HeaderWorkerKind = "x-digitorn-worker-kind"

// HeaderRequestID propagates a correlation ID across daemon→worker.
const HeaderRequestID = "x-digitorn-request-id"

// readyLinePrefix : the worker writes "<prefix><addr>" once it binds the
// gRPC listener. The master parses stdout for this line to learn the
// dynamically-assigned port (Spec uses port 0 → OS picks).
const readyLinePrefix = "DIGITORN_WORKER_READY="

// HandshakeContext provides values the master injects into the worker via
// env vars : the secret + the bind address (port 0 → OS picks). The
// worker calls ReadEnvHandshake() at startup to recover them.
type HandshakeContext struct {
	Secret string
	Kind   Kind
}

// ReadEnvHandshake is called by a worker binary at startup to obtain the
// secret/kind passed by the master via env vars.
func ReadEnvHandshake() (HandshakeContext, error) {
	secret := envOr(EnvSecretKey, "")
	if secret == "" {
		return HandshakeContext{}, ErrAuthBadSecret
	}
	return HandshakeContext{
		Secret: secret,
		Kind:   Kind(envOr("DIGITORN_WORKER_KIND", "unknown")),
	}, nil
}

func envOr(key, def string) string {
	v := getenv(key)
	if v == "" {
		return def
	}
	return v
}
