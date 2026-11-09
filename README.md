# modelGateway

一个模型网关可运行多个 Go 副本，共享 PostgreSQL。Hertz 处理 HTTP，Ent 定义两张业务表 `model_instances` 和 `requests`。网关持有队列和容量，模型执行端确认终态并上传成品，ModelHub 使用 OSS/S3 对象引用读取。

## 调用协议

生成接口为 `POST /v1/gateway/requests/{model_key}`。请求使用 `Authorization: Bearer <api_token>`、`Content-Type: application/json` 和 UUID `Idempotency-Key`。UUID 同时是请求表主键和模型端查询 ID。

请求体包含 `input` 和可选的 `metadata`。图片按顺序放入 `input.images`，每项为 `mime_type` 和 base64 `data`。同一键和相同业务输入复用原任务，改模型或业务输入返回 409。追踪字段变化不改变任务身份。

| 模型 | 输入 |
|---|---|
| `Vmind_wardrobe_v1.0` | 原图与衣物名称，可带角色、框、解析标签或第二张解析掩码。按业务输入选择商品图或拆衣。 |
| `Vmind_outfit_v1.0` | 人物图、衣物图和提示词。 |
| `Vmind_bodymodel_v1.0` | 人物图与性别。 |
| `Vmind_outfit_video_v1.0` | 首帧、提示词和可选动作参数。 |

图片持久入队后同步等待结果。视频持久入队后立即返回 202 和 `request_id`、`state`。使用相同请求头调用 `GET /v1/gateway/requests` 查询。成功响应为 `result.files[]`，每项只有 `provider`、`region`、`bucket` 和 `key`。在排队时可用 `POST /v1/gateway/requests/cancel` 取消，已经派发返回 409。

队列按模型限等待条数和字节数，默认 64 条、128 MiB。队列满返回 429。没有健康实例或 PG 不可用返回 503。排队与首次模型调用共用默认 120 秒。

## 实例管理

`POST /v1/instances/register` 使用注册或管理凭据，提交以下字段。

```json
{
  "instance_id": "g7e-vton-a",
  "model_key": "Vmind_outfit_v1.0",
  "capability": "vton",
  "endpoint": "http://10.0.1.10:8001",
  "health_url": "http://10.0.1.10:8001/health/ready",
  "artifact_digest": "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "resource_group": "g7e/gpu0",
  "group_capacity_units": 1,
  "capacity_units": 1
}
```

网关主动探测。探针返回 `ready: true` 后实例可以接单。使用管理凭据调用 `GET /v1/instances` 查询和 `POST /v1/instances/{id}/drain` 设置 `{"draining":true}`。同一卡的模型登记同一个资源组和组容量。部署身份变化时先排空旧实例，再登记新实例。

## 处理和恢复

`requests` 保存原始业务 JSON、四态、截止时间、实际派发实例和制品、容量预留、后端任务及对象结果。实例和组占用从仍预留槽位的请求汇总。并发受理与派发使用可串行化事务。消费者在持有任务行锁时提交归属和容量，再调用模型。

派发使用 `POST {endpoint}/requests`。模型端接收 `request_id`、`model_key`、`capability`、`input` 和 `metadata`。返回同一 `request_id`，状态为 `processing`、`succeeded` 或 `failed`。未受理的输入校验和鉴权错误使用 HTTP 400、413、415、401 或 403。已经受理的任务通过 `GET {endpoint}/requests/{request_id}` 查询，完成状态必须提供对象结果或结构化错误。

网关的状态为 `queued`、`processing`、`succeeded`、`failed`。副本退出后其他副本接管队列和状态查询。派发结果未知时保留槽位，查询原实例，禁止重发生成。确认终态后原子释放，迟到的确认不会改写已经对外失败的状态。

## 模型侧适配器

`worker_adapter/app.py` 在模型服务旁运行，复用 VMind-Image 的图片 multipart 路由和 LTX 的执行、任务查询及视频读取。适配器在本地目录保存受理和结果记录，上传成功后返回对象引用。用同一部署实例身份重启时保留该目录，每个目录只运行一个适配器进程。已发送图片请求在适配器自身中断且缺少后端终态时保持占槽，需核对模型执行事实。

部署配置提供 `VMIND_WORKER_STATE_DIR`、`VMIND_WORKER_BEARER`、`VMIND_WORKER_CAPABILITIES`、`VMIND_WORKER_INSTANCE_ID`、`VMIND_WORKER_ARTIFACT_DIGEST`、`VMIND_NATIVE_ORIGIN` 和 `VMIND_NATIVE_HEALTH_URL`。按模型服务需要配置 `VMIND_NATIVE_TOKEN`。对象存储使用 `VMIND_RESULT_PROVIDER`、`VMIND_RESULT_REGION`、`VMIND_RESULT_BUCKET` 和可选端点 `VMIND_RESULT_ENDPOINT`。OSS 配置端点和运行时 OSS 凭据。S3 使用 AWS 凭据链。

```sh
python -m pip install -r worker_adapter/requirements.txt
python -m uvicorn app:app --app-dir worker_adapter --host 127.0.0.1 --port 8001
```

## 网关启动

业务配置通过 AppConfig Agent 或 Nacos 加载。示例见 [gateway.example.json](config/gateway.example.json)。多个副本使用同一份 PG schema。换库前需要排空任务并重启。

```sh
go build -o bin/model-gateway ./cmd/model-gateway
bin/model-gateway -config /etc/model-gateway/bootstrap.json -migrate
bin/model-gateway -config /etc/model-gateway/bootstrap.json
```

`/_gateway/live` 检查进程，`/_gateway/ready` 检查 PG 表。使用名称以 `_test` 结尾的独立 PostgreSQL 运行集成测试。

```sh
TEST_DATABASE_URL=<test-dsn> go test -race ./...
```
