package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/wgdl666/modelGateway/ent"
)

type outputQueryDriver struct {
	dialect.Driver
	query string
	stop  error
}

func (d *outputQueryDriver) Dialect() string { return dialect.Postgres }
func (d *outputQueryDriver) Query(_ context.Context, query string, _ any, _ any) error {
	d.query = query
	return d.stop
}

// 检查成品读取实际生成的投影，防止后续把大入参加回输出预览查询。
func TestRequestOutputDoesNotLoadPayload(t *testing.T) {
	d := &outputQueryDriver{stop: errors.New("query captured")}
	s := New(ent.NewClient(ent.Driver(d)))
	_, err := s.RequestOutput(t.Context(), "test-id")
	if !errors.Is(err, d.stop) {
		t.Fatalf("query not captured: %v", err)
	}
	if !strings.HasPrefix(d.query, `SELECT "requests"."request_id", "requests"."result" FROM`) || strings.Contains(d.query, "request_payload") {
		t.Fatalf("unexpected output projection: %s", d.query)
	}
}
