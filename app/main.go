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

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		log.Error("cannot create a pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

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

	// Load-target endpoint. One DB call, no retry — a 200 proves ALB→Fargate→RDS.
	r.Get("/echo", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()

		var dbTime time.Time
		if err := dbAttempt(ctx, pool, log, "db", 1, "SELECT now()", &dbTime); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": os.Getenv("SERVICE_ROLE"),
			"db_time": dbTime.Format(time.RFC3339Nano),
		})
	})

	log.Info("listening", "addr", ":8080", "role", os.Getenv("SERVICE_ROLE"))

	handler := otelhttp.NewHandler(r, "http.server")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
