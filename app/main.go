package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	maxAttempts       = 3               // Layer 1: ONE DB-retry layer, three attempts
	perAttemptTimeout = 2 * time.Second // < 3000ms injected latency → each attempt times out under fault
	baseBackoff       = 100 * time.Millisecond

	// Layer 2 (Module 07, edge role): the edge retries the whole downstream call.
	downstreamMaxAttempts = 3               // three edge→downstream attempts
	downstreamTimeout     = 8 * time.Second // MUST exceed one full Layer-1 cascade (~6.3s) so the
	//                                         downstream completes all 3 DB attempts before Layer 2
	//                                         gives up. Shorter here truncates the cascade (§V.4).
)

// server holds the dependencies shared by the HTTP handlers.
type server struct {
	pool       *pgxpool.Pool
	rdb        *redis.Client
	log        *slog.Logger
	role       string
	gatesCache bool

	// Multi-layer cascade (Module 07): when downstreamURL is set, the edge plays
	// the proxy role and runs Layer 2 (echoViaDownstream) instead of the Layer-1
	// DB path. Empty → Module 06 single-layer behaviour (queryNow directly).
	downstreamURL string
	httpClient    *http.Client
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Password is injected as a secret; everything else is plain env. Build the DSN
	// via net/url so the generated password (which can contain URL-reserved chars
	// like ^ = ,) is percent-encoded correctly.
	dsn := (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD")),
		Host:   net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT")),
		Path:   os.Getenv("DB_NAME"),
	}).String()

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		log.Error("cannot create a pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Cache client (soft dependency). go-redis dials lazily, so the handlers only
	// touch it when they run; a blackholed cache fails within their ctx budgets.
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("CACHE_HOST")})
	defer func() { _ = rdb.Close() }()

	role := os.Getenv("SERVICE_ROLE")
	tp, err := initTracer(context.Background(), "cascade-"+role)
	if err != nil {
		log.Error("tracer init failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	srv := &server{
		pool:       pool,
		rdb:        rdb,
		log:        log,
		role:       role,
		gatesCache: os.Getenv("READY_GATES_CACHE") == "true",
		// DOWNSTREAM_URL is set only on the edge in the multi-layer topology
		// (Module 07). The otelhttp transport propagates the W3C traceparent so
		// one trace spans all three layers.
		downstreamURL: os.Getenv("DOWNSTREAM_URL"),
		httpClient:    &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
	}

	log.Info("listening", "addr", ":8080", "role", role)
	handler := otelhttp.NewHandler(srv.routes(), "http.server")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func (s *server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID) // request_id for the amplification query
	r.Use(middleware.Recoverer)
	r.Get("/health", s.handleHealth)
	r.Get("/echo", s.handleEcho)
	r.Get("/ready", s.handleReady)
	return r
}

// handleHealth is liveness only: the process is up, validating NO dependency.
// This naive 200 is the anti-baseline the health-check track attacks.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleEcho dispatches by topology. If a downstream is wired (edge role in the
// multi-layer cascade, Module 07), proxy to it with the Layer-2 retry; otherwise
// run the Module 06 single-layer DB path directly (queryNow). ONE binary serves
// both experiments, and the layer count stays honest — the edge never runs BOTH
// the proxy retry and a DB retry (that would be a 4th layer, the >40× falsifier).
//
// The Module 05 cache-aside stays bypassed for the retry track (cache unused
// here, Module 06 §I): a cache hit would skip the DB and mask the amplification.
func (s *server) handleEcho(w http.ResponseWriter, req *http.Request) {
	if s.downstreamURL != "" {
		s.echoViaDownstream(w, req) // Layer 2 (Module 07, edge role)
		return
	}
	// Layer 1 only (Module 06 path): budget generous for 3 DB attempts + backoff.
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	dbTime, err := s.queryNow(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"service": s.role, "db_time": dbTime})
}

// echoViaDownstream is Layer 2: the edge retries the whole downstream call up to
// 3 times. Each attempt is one HTTP call to downstream /echo, which itself runs
// Layer 1 (up to 3 DB attempts). So one edge request drives up to 3 × 3 = 9
// db_attempt lines; the k6 client retry (Layer 3) triples that to 27.
func (s *server) echoViaDownstream(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second) // ≥ 3 Layer-2 attempts + backoff
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= downstreamMaxAttempts; attempt++ {
		actx, acancel := context.WithTimeout(ctx, downstreamTimeout)
		status, body, err := s.callDownstream(actx, req)
		acancel()
		if err == nil && status == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		if status >= 400 && status < 500 { // THE GATE (Cluster 1 §VIII) — client errors will fail again
			http.Error(w, "downstream client error", status)
			return
		}
		lastErr = err
		if attempt < downstreamMaxAttempts {
			time.Sleep(jitter(baseBackoff << (attempt - 1))) // full jitter, reused from Module 06
		}
	}
	s.log.Warn("layer-2 exhausted", "event", "downstream_exhausted",
		"request_id", middleware.GetReqID(req.Context()), "err", lastErr)
	http.Error(w, "downstream error", http.StatusInternalServerError)
}

// callDownstream issues ONE Layer-2 attempt against downstream /echo. It forwards
// X-Request-Id so chi's RequestID middleware on the downstream REUSES the origin
// id instead of minting a fresh one — that reuse is what makes the amplification
// query count attempts per ORIGINATING request (§VI.2). The otelhttp transport
// propagates the traceparent so all layers share one trace.
func (s *server) callDownstream(ctx context.Context, orig *http.Request) (int, []byte, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, s.downstreamURL+"/echo", nil)
	if err != nil {
		return 0, nil, err
	}
	r.Header.Set("X-Request-Id", middleware.GetReqID(orig.Context())) // origin-id propagation
	resp, err := s.httpClient.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// queryNow runs the single-layer retry: up to maxAttempts DB attempts, each on
// its own perAttemptTimeout budget, with exponential backoff between. Each
// attempt emits a db_attempt line, so under the DB-latency fault (3s >
// perAttemptTimeout) every attempt times out and the retries ARE the
// amplification the retry track measures.
func (s *server) queryNow(ctx context.Context) (time.Time, error) {
	var dbTime time.Time
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		actx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		err = dbAttempt(actx, s.pool, s.log, "db", attempt, "SELECT now()", &dbTime)
		cancel()
		if err == nil {
			return dbTime, nil
		}
		if !isRetryable(err) { // THE GATE (Cluster 1 §VIII) — never retry client-error classes
			return dbTime, err
		}
		if attempt < maxAttempts {
			// Exponential backoff with FULL jitter (Brooker): sleep ∈ [0, base·2^(n-1)].
			time.Sleep(jitter(baseBackoff << (attempt - 1)))
		}
	}
	return dbTime, err
}

// isRetryable gates retries to transient failures: the per-attempt context
// deadline (our timeout under the latency fault) and pgconn-safe errors are
// retryable; client-error classes are not.
func isRetryable(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || pgconn.SafeToRetry(err)
}

// jitter returns a random duration in [0, d) — full jitter.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d)))
}

// handleReady gates HARD dependencies only, UNLESS the trap is armed. The DB
// pool check is always present (Module 04); READY_GATES_CACHE=true adds the soft
// cache dep as if it were hard (the Module 05 mis-design).
func (s *server) handleReady(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 1*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil { // hard dep — correct
		http.Error(w, "not ready: db", http.StatusServiceUnavailable)
		return
	}
	if s.gatesCache { // THE MIS-DESIGN — soft dep gated as hard
		cctx, ccancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer ccancel()
		if err := s.rdb.Ping(cctx).Err(); err != nil {
			s.log.Warn("readiness failing on SOFT dep", "event", "ready_fail_cache", "err", err)
			http.Error(w, "not ready: cache", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
