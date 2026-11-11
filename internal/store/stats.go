package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/predicate"
	"github.com/wgdl666/modelGateway/ent/request"
)

// StatsBuckets 固定分桶数，时间窗越长每桶越宽，响应大小与查询代价不随窗口增长。
const StatsBuckets = 24

// StatsFailureLimit 只返回最多的失败组合，罕见错误仍可在请求列表按状态筛出。
const StatsFailureLimit = 20

type ModelStats struct {
	ModelKey     string   `json:"model_key"`
	Total        int      `json:"total"`
	Succeeded    int      `json:"succeeded"`
	Failed       int      `json:"failed"`
	Queued       int      `json:"queued"`
	Processing   int      `json:"processing"`
	ElapsedP50MS *float64 `json:"elapsed_p50_ms"`
	ElapsedP95MS *float64 `json:"elapsed_p95_ms"`
	QueueP50MS   *float64 `json:"queue_p50_ms"`
	QueueP95MS   *float64 `json:"queue_p95_ms"`
	SeriesTotal  []int    `json:"series_total"`
	SeriesFailed []int    `json:"series_failed"`
}

type FailureGroup struct {
	ErrorCode     string    `json:"error_code"`
	ModelKey      string    `json:"model_key"`
	InstanceID    string    `json:"instance_id"`
	Count         int       `json:"count"`
	LastAt        time.Time `json:"last_at"`
	SampleID      string    `json:"sample_request_id"`
	SampleMessage string    `json:"sample_message"`
}

type RequestStats struct {
	From          time.Time      `json:"from"`
	To            time.Time      `json:"to"`
	BucketSeconds int64          `json:"bucket_seconds"`
	Models        []ModelStats   `json:"models"`
	Failures      []FailureGroup `json:"failures"`
}

func raw(expr string) ent.AggregateFunc {
	return func(*entsql.Selector) string { return expr }
}

func at(t time.Time) string {
	// 边界由服务端解析后的时间生成，只拼接整数，不拼接请求文本。
	return fmt.Sprintf("to_timestamp(%d.%06d)", t.Unix(), t.Nanosecond()/1000)
}

func bucketSeries(from time.Time, bucket time.Duration, filter string) string {
	parts := make([]string, StatsBuckets)
	for i := range parts {
		start, end := from.Add(time.Duration(i)*bucket), from.Add(time.Duration(i+1)*bucket)
		parts[i] = fmt.Sprintf("count(*) FILTER (WHERE %screated_at >= %s AND created_at < %s)", filter, at(start), at(end))
	}
	return "json_build_array(" + strings.Join(parts, ", ") + ")::text"
}

// RequestStats 汇总时间窗内各模型的量、成功率与耗时分位，排队与执行中按当前积压统计，不受时间窗限制。
func (s *Store) RequestStats(ctx context.Context, from, to time.Time) (*RequestStats, error) {
	bucket := to.Sub(from) / StatsBuckets
	window := []predicate.Request{request.CreatedAtGTE(from), request.CreatedAtLT(to)}
	var windowRows []struct {
		ModelKey     string   `json:"model_key"`
		Total        int      `json:"total"`
		Succeeded    int      `json:"succeeded"`
		Failed       int      `json:"failed"`
		ElapsedP50MS *float64 `json:"elapsed_p50_ms"`
		ElapsedP95MS *float64 `json:"elapsed_p95_ms"`
		QueueP50MS   *float64 `json:"queue_p50_ms"`
		QueueP95MS   *float64 `json:"queue_p95_ms"`
		SeriesTotal  string   `json:"series_total"`
		SeriesFailed string   `json:"series_failed"`
	}
	elapsed := "extract(epoch FROM finished_at - created_at) * 1000"
	queue := "extract(epoch FROM started_at - created_at) * 1000"
	err := s.db.Request.Query().Where(window[0], window[1]).GroupBy(request.FieldModelKey).Aggregate(
		ent.As(ent.Count(), "total"),
		ent.As(raw("count(*) FILTER (WHERE state = 'succeeded')"), "succeeded"),
		ent.As(raw("count(*) FILTER (WHERE state = 'failed')"), "failed"),
		// 只用成功请求算执行耗时，失败多为超时，会把分位拉到截止时间。
		ent.As(raw("percentile_cont(0.5) WITHIN GROUP (ORDER BY "+elapsed+") FILTER (WHERE state = 'succeeded')"), "elapsed_p50_ms"),
		ent.As(raw("percentile_cont(0.95) WITHIN GROUP (ORDER BY "+elapsed+") FILTER (WHERE state = 'succeeded')"), "elapsed_p95_ms"),
		ent.As(raw("percentile_cont(0.5) WITHIN GROUP (ORDER BY "+queue+")"), "queue_p50_ms"),
		ent.As(raw("percentile_cont(0.95) WITHIN GROUP (ORDER BY "+queue+")"), "queue_p95_ms"),
		ent.As(raw(bucketSeries(from, bucket, "")), "series_total"),
		ent.As(raw(bucketSeries(from, bucket, "state = 'failed' AND ")), "series_failed"),
	).Scan(ctx, &windowRows)
	if err != nil {
		return nil, err
	}
	models := map[string]*ModelStats{}
	for _, row := range windowRows {
		item := &ModelStats{
			ModelKey: row.ModelKey, Total: row.Total, Succeeded: row.Succeeded, Failed: row.Failed,
			ElapsedP50MS: row.ElapsedP50MS, ElapsedP95MS: row.ElapsedP95MS, QueueP50MS: row.QueueP50MS, QueueP95MS: row.QueueP95MS,
		}
		if err := json.Unmarshal([]byte(row.SeriesTotal), &item.SeriesTotal); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(row.SeriesFailed), &item.SeriesFailed); err != nil {
			return nil, err
		}
		models[row.ModelKey] = item
	}

	var backlog []struct {
		ModelKey string `json:"model_key"`
		State    string `json:"state"`
		Count    int    `json:"count"`
	}
	err = s.db.Request.Query().Where(request.StateIn(request.StateQueued, request.StateProcessing)).
		GroupBy(request.FieldModelKey, request.FieldState).Aggregate(ent.As(ent.Count(), "count")).Scan(ctx, &backlog)
	if err != nil {
		return nil, err
	}
	for _, row := range backlog {
		item := models[row.ModelKey]
		if item == nil {
			item = &ModelStats{ModelKey: row.ModelKey, SeriesTotal: make([]int, StatsBuckets), SeriesFailed: make([]int, StatsBuckets)}
			models[row.ModelKey] = item
		}
		if row.State == string(request.StateQueued) {
			item.Queued = row.Count
		} else {
			item.Processing = row.Count
		}
	}

	var failures []FailureGroup
	err = s.db.Request.Query().Where(window[0], window[1], request.StateEQ(request.StateFailed)).
		GroupBy(request.FieldErrorCode, request.FieldModelKey, request.FieldInstanceID).Aggregate(
		ent.As(ent.Count(), "count"),
		ent.As(raw("max(created_at)"), "last_at"),
		ent.As(raw("(array_agg(request_id ORDER BY created_at DESC))[1]"), "sample_request_id"),
		ent.As(raw("left((array_agg(error_message ORDER BY created_at DESC))[1], 300)"), "sample_message"),
	).Scan(ctx, &failures)
	if err != nil {
		return nil, err
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].Count != failures[j].Count {
			return failures[i].Count > failures[j].Count
		}
		return failures[i].LastAt.After(failures[j].LastAt)
	})
	if len(failures) > StatsFailureLimit {
		failures = failures[:StatsFailureLimit]
	}

	result := &RequestStats{From: from, To: to, BucketSeconds: int64(bucket / time.Second), Models: []ModelStats{}, Failures: failures}
	if result.Failures == nil {
		result.Failures = []FailureGroup{}
	}
	for _, item := range models {
		result.Models = append(result.Models, *item)
	}
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].ModelKey < result.Models[j].ModelKey })
	return result, nil
}
