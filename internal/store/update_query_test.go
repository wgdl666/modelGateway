package store

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
)

type updateOnlyDriver struct {
	dialect.Driver
	t        *testing.T
	affected int64
	calls    int
}

func (d *updateOnlyDriver) Dialect() string { return dialect.Postgres }
func (d *updateOnlyDriver) Exec(_ context.Context, query string, _ any, result any) error {
	d.calls++
	if !strings.HasPrefix(query, "UPDATE ") || strings.Contains(query, "RETURNING") || strings.Contains(query, "request_payload") {
		d.t.Fatalf("unexpected update SQL: %s", query)
	}
	*result.(*sql.Result) = driver.RowsAffected(d.affected)
	return nil
}
func (d *updateOnlyDriver) Query(_ context.Context, query string, _ any, _ any) error {
	d.t.Fatalf("unexpected readback: %s", query)
	return nil
}

// 验证当前 Ent 生成代码只执行 UPDATE 并返回影响行数，不隐式读回实体。
func TestRequestUpdateDoesNotReadBack(t *testing.T) {
	for _, affected := range []int64{0, 1} {
		d := &updateOnlyDriver{t: t, affected: affected}
		client := ent.NewClient(ent.Driver(d))
		count, err := client.Request.Update().Where(request.IDEQ("test-id")).SetPollClaim("").Save(t.Context())
		if err != nil || count != int(affected) || d.calls != 1 {
			t.Fatalf("affected=%d: count=%d calls=%d err=%v", affected, count, d.calls, err)
		}
	}
}
