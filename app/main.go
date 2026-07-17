package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Password is injected as a secret; everything else is plain env.
	// Build the DSN via net/url so the generated password (which can contain
	// URL-reserved chars like ^ = ,) is percent-encoded correctly.
	dsn := (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD")),
		Host:   net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT")),
		Path:   os.Getenv("DB_NAME"),
	}).String()

	gatesCache := os.Getenv("READY_GATES_CACHE") == "true"
	role := os.Getenv("SERVICE_ROLE")

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

	tp, err := initTracer(context.Background(), "cascade-"+os.Getenv("SERVICE_ROLE"))
	if err != nil {
		log.Error("tracer init failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	r := chi.NewRouter()
	r.Use(middleware.RequestID) // request_id for the amplification query
	r.Use(middleware.Recoverer)

	// Liveness only: the process is up. Validates NO dependency.
	// This naive 200 is the anti-baseline the health-check track attacks.
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// /echo — cache is a SOFT dependency (cache-aside). A cache miss OR a cache
	// OUTAGE falls through to the DB; the request still returns 200. "Soft" = the
	// request degrades (cache bypassed, one extra DB hit), it does not fail.
	r.Get("/echo", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()

		// Best-effort cache read with a SHORT budget so an outage can't slow /echo much.
		cctx, ccancel := context.WithTimeout(ctx, 300*time.Millisecond)
		if v, err := rdb.Get(cctx, "now").Result(); err == nil {
			ccancel()
			writeJSON(w, map[string]any{"service": role, "db_time": v, "cache": "hit"})
			return
		}
		ccancel() // miss or cache down — fall through, do NOT fail

		var dbTime time.Time
		if err := dbAttempt(ctx, pool, log, "db", 1, "SELECT now()", &dbTime); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		_ = rdb.Set(context.Background(), "now", dbTime.Format(time.RFC3339Nano), 5*time.Second).Err() // best-effort repopulate
		writeJSON(w, map[string]any{"service": role, "db_time": dbTime, "cache": "miss"})
	})

	// /ready — HARD deps only, UNLESS the trap is armed. The DB pool check is always
	// present (Module 04). READY_GATES_CACHE=true adds the soft cache dep as if hard.
	r.Get("/ready", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 1*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil { // hard dep — correct
			http.Error(w, "not ready: db", http.StatusServiceUnavailable)
			return
		}
		if gatesCache { // THE MIS-DESIGN — soft dep gated as hard
			cctx, ccancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer ccancel()
			if err := rdb.Ping(cctx).Err(); err != nil {
				log.Warn("readiness failing on SOFT dep", "event", "ready_fail_cache", "err", err)
				http.Error(w, "not ready: cache", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	log.Info("listening", "addr", ":8080", "role", os.Getenv("SERVICE_ROLE"))

	handler := otelhttp.NewHandler(r, "http.server")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
