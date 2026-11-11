package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestObserveRequestLinksParentAndOmitsSensitiveData(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous, logger := otel.GetTracerProvider(), slog.Default()
	otel.SetTracerProvider(provider)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		slog.SetDefault(logger)
		_ = provider.Shutdown(context.Background())
	})
	h := server.New()
	h.Use(observeRequest)
	h.POST("/v1/models/:id", func(_ context.Context, c *app.RequestContext) { c.Status(503) })
	h.GET("/_gateway/live", func(_ context.Context, c *app.RequestContext) { c.Status(200) })
	const parentTrace = "1234567890abcdef1234567890abcdef"
	response := ut.PerformRequest(h.Engine, "POST", "/v1/models/private-id?token=secret-query", &ut.Body{Body: strings.NewReader("secret-body"), Len: 11},
		ut.Header{Key: "Authorization", Value: "Bearer secret-token"},
		ut.Header{Key: "traceparent", Value: "00-" + parentTrace + "-1234567890abcdef-01"})
	if response.Code != 503 {
		t.Fatalf("status=%d", response.Code)
	}
	ut.PerformRequest(h.Engine, "GET", "/_gateway/live", nil)
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].SpanContext().TraceID().String() != parentTrace || spans[0].Parent().SpanID().String() != "1234567890abcdef" {
		t.Fatalf("request must keep the upstream parent and skip health probes: %v", spans)
	}
	if spans[0].Status().Code != codes.Error || spans[0].Name() != "gateway.http_handler" {
		t.Fatalf("failed handler span = %s / %v", spans[0].Name(), spans[0].Status())
	}
	attrs := fmt.Sprint(spans[0].Attributes())
	if !strings.Contains(attrs, "/v1/models/:id") || strings.Contains(attrs, "secret") || strings.Contains(attrs, "private-id") {
		t.Fatalf("must record route template without request data: %s", attrs)
	}
}
