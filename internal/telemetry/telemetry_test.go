package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wgdl666/modelGateway/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSetupWithoutTokenUsesGatewayIdentity(t *testing.T) {
	logger := slog.Default()
	traceProvider, logProvider, propagator := otel.GetTracerProvider(), global.GetLoggerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		otel.SetTracerProvider(traceProvider)
		global.SetLoggerProvider(logProvider)
		otel.SetTextMapPropagator(propagator)
	})
	runtime, err := Setup(t.Context(), config.LogfireConfig{Env: "local-test", Version: "test-version", OtelLogLevel: "info"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := otel.Tracer("test").Start(t.Context(), "gateway.test")
	slog.InfoContext(ctx, "gateway test event")
	resource := map[string]string{}
	for _, attr := range span.(sdktrace.ReadOnlySpan).Resource().Attributes() {
		resource[string(attr.Key)] = attr.Value.AsString()
	}
	span.End()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resource["service.name"] != "model-gateway" || resource["deployment.environment.name"] != "local-test" || resource["service.version"] != "test-version" {
		t.Fatalf("wrong service identity: %v", resource)
	}
}

func TestLogErrorKeepsSQLStateWithoutLeakingDriverDetails(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	err := fmt.Errorf("connection string secret-password: %w", &pgconn.PgError{Code: "42703", TableName: "requests", ColumnName: "protocol", Message: "secret-message", Detail: "secret-row", Where: "secret-query"})
	LogError(t.Context(), "gateway readiness failed", err, "check", "requests.protocol")
	LogError(t.Context(), "gateway database connection failed", errors.Join(context.DeadlineExceeded, errors.New("secret-password")))
	raw := output.String()
	if strings.Contains(raw, "secret") || !strings.Contains(raw, `"db.sqlstate":"42703"`) || !strings.Contains(raw, `"db.column":"protocol"`) || !strings.Contains(raw, `"reason":"deadline_exceeded"`) {
		t.Fatalf("unsafe or incomplete database diagnostic: %s", raw)
	}
}
