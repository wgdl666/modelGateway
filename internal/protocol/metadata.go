package protocol

import (
	"context"
	"github.com/wgdl666/kangaroo/callmeta"
	"go.opentelemetry.io/otel/trace"
)

// CaptureMetadata 冻结当前执行来源；不参与业务 input 和幂等比较。
func CaptureMetadata(ctx context.Context) Metadata {
	values := callmeta.Capture(ctx)
	device, user := callmeta.Identity(ctx)
	result := Metadata{DeviceID: device, UserID: user, TraceParent: values["traceparent"], TraceState: values["tracestate"]}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		result.TraceID = span.TraceID().String()
	}
	return result
}

// Restore 只恢复持久来源，不携带原连接 deadline；空身份清除查询者的身份。
func (m Metadata) Restore(ctx context.Context) context.Context {
	return callmeta.Restore(ctx, map[string]string{
		callmeta.DeviceID: m.DeviceID, callmeta.UserID: m.UserID,
		"traceparent": m.TraceParent, "tracestate": m.TraceState,
	})
}
