package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/kangaroo/callmeta"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func observeRequest(ctx context.Context, c *app.RequestContext) {
	route := c.FullPath()
	// 高频健康探针只在失败时输出诊断，避免正常探活淹没业务链路。
	if route == "/_gateway/live" || route == "/_gateway/ready" {
		c.Next(ctx)
		return
	}
	if route == "" {
		route = "unmatched"
	}
	// 只接收标准 Trace 与固定来源字段，不复制任意请求头或 baggage。
	ctx = (callmeta.Propagator{}).Extract(ctx, propagation.MapCarrier{
		"traceparent": string(c.GetHeader("traceparent")), "tracestate": string(c.GetHeader("tracestate")),
		callmeta.DeviceID: string(c.GetHeader(callmeta.DeviceID)), callmeta.UserID: string(c.GetHeader(callmeta.UserID)),
	})
	ctx, span := otel.Tracer("github.com/wgdl666/modelGateway").Start(ctx, "gateway.http_handler",
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
			attribute.String("http.request.method", string(c.Method())), attribute.String("http.route", route)))
	defer span.End()
	callmeta.AnnotateSpan(ctx)
	started := time.Now()
	c.Next(ctx)
	status := c.Response.StatusCode()
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	if status >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
	if status >= http.StatusBadRequest {
		// Hertz 的流式 body 此时尚未读完，这里只报告 handler/响应头阶段，不冒充模型终态。
		slog.WarnContext(ctx, "gateway request rejected", "http.route", route, "http.status_code", status,
			"handler_elapsed_ms", time.Since(started).Milliseconds())
	}
}
