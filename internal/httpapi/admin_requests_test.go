package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// 旧请求没有缓存输入清单，但已有成品必须直接出现在列表，输入仍可从详情恢复。
func TestListLegacyRequestMedia(t *testing.T) {
	for _, extra := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		row := &ent.Request{
			Protocol:       protocol.ClothGenProtocol,
			Extra:          extra,
			RequestPayload: json.RawMessage(`{"input":{"images":[{"mime_type":"image/png","data":"eA=="}],"garment_name":"shirt"}}`),
			Result:         json.RawMessage(`{"files":[{"provider":"s3","bucket":"private-bucket","key":"outputs/test/0.png"}]}`),
		}
		raw, err := json.Marshal(listRequest(row))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["inputs"]; present {
			t.Fatalf("legacy inputs must be omitted: %s", raw)
		}
		if string(fields["outputs"]) != `[{"index":0,"mime_type":"image/png"}]` {
			t.Fatalf("legacy outputs missing: %s", raw)
		}
		for _, forbidden := range []string{`"result"`, "private-bucket", "outputs/test/0.png", "eA=="} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("list leaked %q: %s", forbidden, raw)
			}
		}
		detail := inspectRequest(row)
		if len(detail.Inputs) != 1 || detail.Inputs[0].Index != 0 || detail.Inputs[0].Role != "image" || detail.Inputs[0].MIMEType != "image/png" {
			t.Fatalf("legacy detail inputs = %+v", detail.Inputs)
		}
	}
}

// 无成品时固定返回空数组；已记录的空输入清单无需触发详情回退。
func TestListRequestWithoutOutputs(t *testing.T) {
	for _, tc := range []struct {
		extra  json.RawMessage
		inputs string
	}{
		{nil, ""},
		{json.RawMessage(`{"input_media":[]}`), "[]"},
	} {
		raw, err := json.Marshal(listRequest(&ent.Request{Extra: tc.extra}))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["outputs"]) != "[]" || string(fields["inputs"]) != tc.inputs {
			t.Fatalf("unexpected empty media: %s", raw)
		}
	}
}
