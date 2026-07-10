package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Password is injected as a secret; everything else is plain env.
	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s",
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASSWORD"),
		os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"),
		os.Getenv("DB_NAME"),
	)

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		log.Error("cannot create a pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	r := chi.NewRouter()
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
		if err := pool.QueryRow(ctx, "SELECT now()").Scan(&dbTime); err != nil {
			log.Error("db query failed", "err", err)
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
	if err := http.ListenAndServe(":8080", r); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}