package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/sony/gobreaker"
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

	// Cascade mitigation (Module 08): non-nil only when EDGE_BREAKER=true. When
	// set, the edge drops the Layer-2 retry loop and instead guards a SINGLE
	// edge→downstream call behind this circuit breaker (echoViaDownstreamBreaker).
	// nil → Module 07 behaviour (echoViaDownstream retry loop).
	breaker *gobreaker.CircuitBreaker
}

// errClientError marks a downstream 4xx: it is surfaced to the caller but must
// NOT count as a breaker failure — the same error gate as the retry loop
// (Cluster 1 §VIII), a client error is not the dependency's fault.
var errClientError = errors.New("downstream client error (4xx)")

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

	// Module 08 mitigation: arm the edge→downstream circuit breaker when flagged.
	if os.Getenv("EDGE_BREAKER") == "true" {
		srv.breaker = newBreaker(log)
		log.Info("edge breaker armed (Module 08 mitigation)")
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
		if s.breaker != nil {
			s.echoViaDownstreamBreaker(w, req) // Module 08: single guarded call, no retry
		} else {
			s.echoViaDownstream(w, req) // Layer 2 (Module 07, edge role)
		}
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

// newBreaker builds the edge→downstream circuit breaker (Module 08, ADR-CRA-011).
// The Settings ARE the teaching payload — every field is explicit, not hidden
// behind a framework. The breaker (not a retry) handles SUSTAINED downstream
// failure: it trips OPEN and sheds to a fast 503, giving the innermost dependency
// room to recover.
func newBreaker(log *slog.Logger) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "edge->downstream",
		MaxRequests: 1,                // half-open: admit exactly ONE probe before deciding
		Interval:    0,                // never auto-reset failure counts while closed — count until we trip
		Timeout:     10 * time.Second, // stay OPEN 10s, then half-open to probe whether downstream recovered
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return c.ConsecutiveFailures >= 5 // trip after 5 straight downstream failures
		},
		IsSuccessful: func(err error) bool {
			// THE GATE, on the breaker: a 5xx or transport error is a breaker
			// failure; a 4xx (errClientError) is NOT — a client error is not the
			// dependency's fault and must not trip the breaker.
			return err == nil || errors.Is(err, errClientError)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			// Emit the transition so the run can plot the closed/open/half-open
			// timeline (§VI.3) — the breaker's engagement is a measured quantity.
			log.Info("breaker state change", "event", "breaker_state",
				"breaker", name, "from", from.String(), "to", to.String())
		},
	})
}

// echoViaDownstreamBreaker is the mitigated Layer 2 (Module 08). There is NO retry
// loop here (that was the 27× amplifier) — ONE call, guarded by the breaker. Layer
// 1 (downstream queryNow) is unchanged and still retries 3×; the mitigation removes
// the OUTER retries and lets the breaker handle sustained failure by shedding.
func (s *server) echoViaDownstreamBreaker(w http.ResponseWriter, req *http.Request) {
	body, err := s.breaker.Execute(func() (any, error) {
		ctx, cancel := context.WithTimeout(req.Context(), downstreamTimeout) // ONE attempt, no retry
		defer cancel()
		status, b, callErr := s.callDownstream(ctx, req)
		switch {
		case callErr != nil:
			return nil, callErr // transport/timeout → breaker failure
		case status >= 500:
			return nil, fmt.Errorf("downstream %d", status) // 5xx → breaker failure
		case status >= 400:
			return b, errClientError // 4xx → surface it, but do NOT trip the breaker (the gate)
		default:
			return b, nil // 2xx → success
		}
	})

	switch {
	case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
		// Breaker OPEN (or half-open probe budget spent): fail fast. NO downstream
		// call was made → 0 db_attempt lines. This shed is the breaker's
		// distinctive contribution beyond removing the outer retries.
		http.Error(w, "circuit open", http.StatusServiceUnavailable)
	case errors.Is(err, errClientError):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body.([]byte)) // pass the 4xx body through unchanged
	case err != nil:
		http.Error(w, "downstream error", http.StatusInternalServerError) // 5xx after a real call
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body.([]byte))
	}
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
