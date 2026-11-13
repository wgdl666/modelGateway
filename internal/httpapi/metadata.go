package httpapi

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/kangaroo/callmeta"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// freezeMetadata 在受理时合并已有 JSON 协议和内部 HTTP 来源；冲突拒绝，不留下两份身份。
// 返回值仅供本 HTTP 适配器转成 invalid_metadata 响应，不越过业务边界。
func freezeMetadata(ctx context.Context, c *app.RequestContext, input protocol.Metadata) (protocol.Metadata, error) {
	device, user := string(c.GetHeader(callmeta.DeviceID)), string(c.GetHeader(callmeta.UserID))
	for _, value := range []string{device, user, input.DeviceID, input.UserID, input.TraceID} {
		if !callmeta.ValidIdentityValue(value) {
			return protocol.Metadata{}, errors.New("invalid metadata")
		}
	}
	if (device != "" && input.DeviceID != "" && device != input.DeviceID) || (user != "" && input.UserID != "" && user != input.UserID) {
		return protocol.Metadata{}, errors.New("conflicting source identity")
	}
	if device == "" {
		device = input.DeviceID
	}
	if user == "" {
		user = input.UserID
	}
	ctx = callmeta.WithIdentity(ctx, device, user)
	callmeta.AnnotateSpan(ctx)
	result := protocol.CaptureMetadata(ctx)
	// traceparent 由 HTTP 服务端 Span 冻结；旧协议显式 trace_id 继续作为账本关联，不伪造父 Span。
	if input.TraceID != "" {
		result.TraceID = input.TraceID
	}
	return result, nil
}
