package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
	"github.com/wgdl666/modelGateway/internal/worker"
)

type requestView struct {
	protocol.Response
	TraceID        string     `json:"trace_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	InstanceID     string     `json:"instance_id,omitempty"`
	ResourceGroup  string     `json:"resource_group,omitempty"`
	ResourceGroups []string   `json:"resource_groups,omitempty"`
	Endpoint       string     `json:"endpoint,omitempty"`
	Artifact       string     `json:"artifact_digest,omitempty"`
	BackendTaskID  string     `json:"backend_task_id,omitempty"`
	SlotReserved   bool       `json:"slot_reserved"`
}

func viewRequest(row *ent.Request) requestView {
	return requestView{
		Response: taskResponse(row), TraceID: row.TraceID,
		CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
		InstanceID: row.InstanceID, ResourceGroup: row.ResourceGroup, ResourceGroups: row.ResourceGroups, Endpoint: row.Endpoint, Artifact: row.ArtifactDigest,
		BackendTaskID: row.BackendTaskID, SlotReserved: row.SlotReserved,
	}
}

// 统计窗口上限 7 天：分位数需要扫描窗口内全部请求，更长的范围应走离线报表。
const maxStatsWindow = 7 * 24 * time.Hour

func (s *Server) requestStats(ctx context.Context, c *app.RequestContext) {
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	for key, target := range map[string]*time.Time{"from": &from, "to": &to} {
		if value := c.Query(key); value != "" {
			bound, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(c, 400, "invalid_time", "stats time range must use RFC3339", "")
				return
			}
			*target = bound
		}
	}
	if window := to.Sub(from); window < time.Hour || window > maxStatsWindow {
		writeError(c, 400, "invalid_time", "stats window must be between 1 hour and 7 days", "")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stats, err := s.store.RequestStats(ctx, from, to)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, stats)
}

func (s *Server) listRequests(ctx context.Context, c *app.RequestContext) {
	limit, errLimit := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, errOffset := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if errLimit != nil || errOffset != nil || limit < 1 || limit > 100 || offset < 0 {
		writeError(c, 400, "invalid_pagination", "invalid request pagination", "")
		return
	}
	filter := store.RequestFilter{
		ID: c.Query("request_id"), TraceID: c.Query("trace_id"), Model: c.Query("model"),
		InstanceID: c.Query("instance_id"), Artifact: c.Query("artifact_digest"),
		Limit: limit, Offset: offset, State: request.State(c.Query("state")),
	}
	if filter.ID != "" && !protocol.ValidID(filter.ID) {
		writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
		return
	}
	if filter.State != "" {
		if err := request.StateValidator(filter.State); err != nil {
			writeError(c, 400, "invalid_state", "invalid request state", "")
			return
		}
	}
	for _, key := range []string{"from", "to"} {
		if value := c.Query(key); value != "" {
			bound, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(c, 400, "invalid_time", "request time range must use RFC3339", "")
				return
			}
			if key == "from" {
				filter.From = &bound
			} else {
				filter.To = &bound
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, total, err := s.store.ListRequests(ctx, filter)
	if err != nil {
		storeError(c, err, "")
		return
	}
	items := make([]listedRequest, 0, len(rows))
	for _, row := range rows {
		items = append(items, listRequest(row))
	}
	writeJSON(c, 200, map[string]any{"items": items, "total": total})
}

// listedRequest 给 Ops 列表直接展示每张输入和成品。请求 extra 里没有输入清单时，
// 仅省略 inputs，表示需要读详情；outputs 始终从结果生成，空数组表示没有成品。
type listedRequest struct {
	requestView
	Inputs  *[]protocol.MediaRef `json:"inputs,omitempty"`
	Outputs []protocol.MediaRef  `json:"outputs"`
}

// inputMedia 只认 extra.input_media。键不存在表示未记录输入清单，空数组表示确认没有输入。
func inputMedia(extra json.RawMessage) ([]protocol.MediaRef, bool) {
	var bag struct {
		InputMedia json.RawMessage `json:"input_media"`
	}
	if len(extra) == 0 || json.Unmarshal(extra, &bag) != nil || bag.InputMedia == nil {
		return nil, false
	}
	refs := []protocol.MediaRef{}
	if json.Unmarshal(bag.InputMedia, &refs) != nil {
		return nil, false
	}
	return refs, true
}

func listRequest(row *ent.Request) listedRequest {
	listed := listedRequest{requestView: viewRequest(row)}
	result := listed.Result
	// 结果里有生成参数，列表只给成品清单，完整结果仍只在详情返回。
	listed.Result = nil
	if inputs, ok := inputMedia(row.Extra); ok {
		listed.Inputs = &inputs
	}
	outputs := []protocol.MediaRef{}
	if result != nil {
		outputs = protocol.OutputMedia(result.Files)
	}
	listed.Outputs = outputs
	return listed
}

type inspectedRequest struct {
	requestView
	Parameters json.RawMessage     `json:"parameters"`
	Inputs     []protocol.MediaRef `json:"inputs"`
	Outputs    []protocol.MediaRef `json:"outputs"`
}

func inspectRequest(row *ent.Request) inspectedRequest {
	view := viewRequest(row)
	parameters := json.RawMessage(`{}`)
	inputs := []protocol.MediaRef{}
	if params, refs, err := protocol.PreviewSubmission(row.Protocol, row.RequestPayload); err == nil {
		parameters = params
		inputs = refs
	}
	if saved, ok := inputMedia(row.Extra); ok {
		inputs = saved
	}
	outputs := []protocol.MediaRef{}
	if view.Result != nil {
		outputs = protocol.OutputMedia(view.Result.Files)
	}
	return inspectedRequest{requestView: view, Parameters: parameters, Inputs: inputs, Outputs: outputs}
}

func (s *Server) requestDetail(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if !protocol.ValidID(id) {
		writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
		return
	}
	row, err := s.store.RequestDetail(ctx, id)
	if err != nil {
		storeError(c, err, id)
		return
	}
	writeJSON(c, 200, inspectRequest(row))
}

const (
	maxImageBytes = 64 << 20
	maxVideoBytes = 200 << 20
)

func (s *Server) objectReader(cfg *config.Snapshot) (objectReader, error) {
	if s.objects != nil {
		return s.objects, nil
	}
	s.objectsOnce.Do(func() {
		s.objects, s.objectsErr = worker.OpenResultStore(cfg.NativeResults)
	})
	return s.objects, s.objectsErr
}

func mediaLimit(mime string) int64 {
	if mime == "video/mp4" {
		return maxVideoBytes
	}
	return maxImageBytes
}

func allowedInputMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func (s *Server) requestMedia(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if !protocol.ValidID(id) {
		writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
		return
	}
	kind := c.Query("kind")
	index, err := strconv.Atoi(c.Query("index"))
	if (kind != "input" && kind != "output") || err != nil || index < 0 {
		writeError(c, 400, "invalid_media", "media kind and index are required", id)
		return
	}
	var row *ent.Request
	if kind == "output" {
		row, err = s.store.RequestOutput(ctx, id)
	} else {
		row, err = s.store.RequestDetail(ctx, id)
	}
	if err != nil {
		storeError(c, err, id)
		return
	}
	switch kind {
	case "input":
		image, err := protocol.PreviewInputImage(row.RequestPayload, index)
		if err != nil || !allowedInputMIME(image.MIMEType) || int64(len(image.Data)) > maxImageBytes {
			writeError(c, 404, "media_unavailable", "input image is unavailable", id)
			return
		}
		writeMedia(c, image.MIMEType, bytes.NewReader(image.Data), int64(len(image.Data)))
	default:
		view := taskResponse(row)
		if view.Result == nil || index >= len(view.Result.Files) {
			writeError(c, 404, "media_unavailable", "output is unavailable", id)
			return
		}
		file := view.Result.Files[index]
		mime := protocol.OutputMIME(file.Key)
		if mime == "" || !protocol.MatchesOutputKey(id, index, file.Key) {
			writeError(c, 404, "media_unavailable", "output is unavailable", id)
			return
		}
		reader, err := s.objectReader(requestConfig(ctx))
		if err != nil {
			writeError(c, 502, "result_unavailable", "result storage is unavailable", id)
			return
		}
		body, size, err := reader.Get(ctx, file)
		if err != nil || size <= 0 || size > mediaLimit(mime) {
			if body != nil {
				_ = body.Close()
			}
			writeError(c, 404, "media_unavailable", "output is unavailable", id)
			return
		}
		writeMedia(c, mime, body, size)
	}
}

func writeMedia(c *app.RequestContext, contentType string, body io.Reader, size int64) {
	c.Response.Header.Set("Content-Type", contentType)
	c.Response.Header.Set("Cache-Control", "private, no-store")
	c.Response.Header.Set("X-Content-Type-Options", "nosniff")
	c.SetStatusCode(200)
	c.Response.SetBodyStream(body, int(size))
}
