package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/kangaroo/callmeta"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"go.opentelemetry.io/otel/trace"
)

func TestFreezeMetadataPersistsHeadersAndRejectsConflicts(t *testing.T) {
	c := new(app.RequestContext)
	c.Request.Header.Set(callmeta.DeviceID, "device-a")
	c.Request.Header.Set(callmeta.UserID, "user-a")
	span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), span)
	for _, input := range []protocol.Metadata{{}, {UserID: "user-a", DeviceID: "device-a"}} {
		frozen, err := freezeMetadata(ctx, c, input)
		if err != nil {
			t.Fatal(err)
		}
		// 同步摘要和异步图片/视频共用 Submission，不需要新增数据库列。
		raw, err := json.Marshal(protocol.Submission{Input: json.RawMessage(`{"prompt":"x"}`), Metadata: frozen})
		if err != nil {
			t.Fatal(err)
		}
		var saved protocol.Submission
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		restored := saved.Metadata.Restore(callmeta.WithIdentity(context.Background(), "different", "different"))
		if device, user := callmeta.Identity(restored); device != "device-a" || user != "user-a" {
			t.Fatal("lost persisted identity")
		}
		if !trace.SpanContextFromContext(restored).Equal(span.WithRemote(true)) {
			t.Fatal("lost parent context")
		}
	}
	if _, err := freezeMetadata(ctx, c, protocol.Metadata{UserID: "other"}); err == nil {
		t.Fatal("accepted conflicting identity")
	}
	if _, err := freezeMetadata(ctx, c, protocol.Metadata{DeviceID: "bad\nheader"}); err == nil {
		t.Fatal("accepted header injection")
	}
}
