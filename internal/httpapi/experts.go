package httpapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// expertSubmission 是通用任务入口。expert 只选择已经登记的专家，不在一张卡上换权重。
type expertSubmission struct {
	Interface string            `json:"interface"`
	Expert    string            `json:"expert"`
	Input     json.RawMessage   `json:"input"`
	Output    string            `json:"output,omitempty"`
	Metadata  protocol.Metadata `json:"metadata,omitempty"`
}

func (s *Server) submitExpert(ctx context.Context, c *app.RequestContext) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	select {
	case s.uploads <- struct{}{}:
	case <-ctx.Done():
		return
	}
	cfg := requestConfig(ctx)
	var body expertSubmission
	if _, ok := readBody(c, cfg.MaxBodyBytes, &body, id); !ok {
		<-s.uploads
		return
	}
	route, err := protocol.ResolveExpert(body.Interface, body.Expert, body.Input, c.GetBool(experimentalCredential))
	if err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", err.Error(), id)
		return
	}
	if body.Output != "" && ((body.Output != "image" && body.Output != "video") || (body.Output == "video") != (route.Protocol == protocol.VideoProtocol)) {
		<-s.uploads
		writeError(c, 400, "invalid_protocol", "model protocol does not match the requested output", id)
		return
	}
	registered, err := s.store.ModelProtocol(ctx, route.ModelKey)
	if errors.Is(err, store.ErrModelUnregistered) {
		<-s.uploads
		writeError(c, 404, "model_not_found", "model has no registration", id)
		return
	}
	if err != nil {
		<-s.uploads
		storeError(c, err, id)
		return
	}
	if registered != route.Protocol {
		<-s.uploads
		writeError(c, 400, "invalid_protocol", "registered protocol does not match the expert route", id)
		return
	}
	s.finishAdmission(ctx, c, id, route.ModelKey, route.Protocol, protocol.Submission{
		Input: route.Input, Output: body.Output, Metadata: body.Metadata,
	})
}
