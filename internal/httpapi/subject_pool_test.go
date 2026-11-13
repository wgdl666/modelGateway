package httpapi

import (
	"io"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestSubjectPoolRoutingIsExact(t *testing.T) {
	const endpoint = "http://10.0.0.2/internal/segment-subject-pool"
	cfg := config.Config{VisionAddr: "172.31.43.7:18280", SubjectPools: map[string]config.SubjectPool{protocol.SegmentSubjectModel: {Protocol: protocol.SegmentSubjectProtocol, Endpoint: endpoint, HealthURL: endpoint + "/ready"}}}
	for _, tc := range []struct {
		model, kind, endpoint, path, want string
		pool                              bool
	}{
		{protocol.SegmentSubjectModel, protocol.SegmentSubjectProtocol, endpoint, "/segment-person/api/segment_subject", endpoint + "/api/segment_subject", true},
		{protocol.SegmentSubjectModel, protocol.SegmentSubjectProtocol, "http://127.0.0.1:6106", "/segment-person/api/segment_subject", "http://172.31.43.7:18280/_dispatch/6106/segment-person/api/segment_subject", false},
		{protocol.SegmentPersonModel, protocol.SegmentPersonProtocol, "http://127.0.0.1:6106", "/segment-person/api/segment", "http://172.31.43.7:18280/_dispatch/6106/segment-person/api/segment", false},
		{protocol.SegmentPersonModel, protocol.SegmentSubjectProtocol, endpoint, "/segment-person/api/segment_subject", "http://172.31.43.7:18280/_dispatch/" + endpoint + "/segment-person/api/segment_subject", false},
	} {
		got, pool := synchronousEndpoint(cfg, &ent.Request{ModelKey: tc.model, Protocol: tc.kind, Endpoint: tc.endpoint}, tc.path)
		if got != tc.want || pool != tc.pool {
			t.Errorf("got %s pool=%v want %s pool=%v", got, pool, tc.want, tc.pool)
		}
	}
}
func TestSynchronousEOFUnknownRetainsReservation(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		calls := 0
		b := &synchronousBody{ReadCloser: io.NopCloser(strings.NewReader("timeout")), unknown: unknown, complete: func(_ protocol.HTTPResult, known bool) {
			calls++
			if known == unknown {
				t.Errorf("unknown=%v known=%v", unknown, known)
			}
		}}
		if _, err := io.Copy(io.Discard, b); err != nil {
			t.Fatal(err)
		}
		b.Read(make([]byte, 1))
		if calls != 1 {
			t.Fatal(calls)
		}
	}

}
