// Package telemetry 复用 ModelHub 的日志导出，并集中限制错误日志可以携带的诊断字段。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wgdl666/kangaroo/logs"
	"github.com/wgdl666/modelGateway/internal/config"
)

func Setup(ctx context.Context, cfg config.LogfireConfig) (*logs.Runtime, error) {
	endpoint := ""
	headers := map[string]string{}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		endpoint = "logfire-us.pydantic.dev"
		headers["Authorization"] = token
	}
	service := strings.TrimSpace(cfg.Service)
	if service == "" {
		service = "model-gateway"
	}
	// 沿用 ModelHub 的 OTLP、鉴权和 slog Handler；尚未配置密钥时保留控制台日志。
	return logs.Setup(ctx, logs.Config{
		ServiceName: service, ServiceVersion: strings.TrimSpace(cfg.Version),
		Environment: strings.TrimSpace(cfg.Env), Endpoint: endpoint, Headers: headers,
		MinLevel: strings.TrimSpace(cfg.OtelLogLevel), Console: true,
	})
}

// LogError 不输出原始 error/SQL/连接串；驱动错误可能携带密码、请求参数或数据库行内容。
func LogError(ctx context.Context, message string, err error, attrs ...any) {
	attrs = append(attrs, "error_type", fmt.Sprintf("%T", err))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		attrs = append(attrs, "db.sqlstate", pgErr.Code, "db.schema", pgErr.SchemaName,
			"db.table", pgErr.TableName, "db.column", pgErr.ColumnName)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		attrs = append(attrs, "reason", "deadline_exceeded")
	} else if errors.Is(err, context.Canceled) {
		attrs = append(attrs, "reason", "canceled")
	}
	slog.ErrorContext(ctx, message, attrs...)
}
