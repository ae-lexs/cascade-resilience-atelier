package main

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("cascade")

// dbAttempt runs exactly ONE database attempt. It emits one span and one
// structured "db_attempt" log line. Counting these lines per request_id, in a
// fault window, IS the DB-call-attempt amplification metric (ADR-CRA-008).
func dbAttempt(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, layer string, attempt int, sql string, dest ...any) error {
	ctx, span := tracer.Start(
		ctx,
		"db.attempt",
		trace.WithAttributes(
			attribute.String("cascade.layer", layer),
			attribute.Int("cascade.attempt", attempt),
		),
	)
	defer span.End()

	err := pool.QueryRow(ctx, sql).Scan(dest...)

	log.Info("db_attempt",
		"event", "db_attempt",
		"request_id", middleware.GetReqID(ctx),
		"layer", layer,
		"attempt", attempt,
		"ok", err == nil,
	)
	if err != nil {
		span.RecordError(err)
	}

	return err
}
