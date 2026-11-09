# modelGateway

一个模型网关可运行多个 Go 副本，共享 PostgreSQL 和 RabbitMQ。Hertz 处理 HTTP，Ent 定义 `model_instances` 和 `requests`。每个副本启动两个 RabbitMQ 消费者。模型执行端确认终态并上传成品，ModelHub 使用 OSS/S3 对象引用读取。

## 调用协议

生成接口为 `POST /v1/gateway/requests/{model_key}`。请求使用 `Authorization: Bearer <api_token>`、`Content-Type: application/json` 和 UUID `Idempotency-Key`。UUID 同时是请求表主键和模型端查询 ID。

请求体包含 `input` 和可选的 `metadata`。图片按顺序放入 `input.images`，每项为 `mime_type` 和 base64 `data`。同一键和相同业务输入复用原任务，改模型或业务输入返回 409。追踪字段变化不改变任务身份。

每个 `model_key` 固定对应一种能力。调用方按业务选择模型键。

| 模型 | 能力 | 输入 |
|---|---|---|
| `Vmind_wardrobe_v1.0` | `garment_extraction` | 人物图、衣物名称和衣物框或人体解析输入。 |
| `Vmind_cloth_gen_v1.0` | `cloth_gen` | 单件原图、衣物名称和可选角色。 |
| `Vmind_outfit_v1.0` | `vton` | 人物图、衣物图和提示词。 |
| `Vmind_bodymodel_v1.0` | `body_model` | 人物图与性别。 |
| `Vmind_outfit_video_v1.0` | `video` | 首帧、提示词和可选动作参数。 |

图片持久入队后同步等待结果。视频持久入队后立即返回 202 和 `request_id`、`state`。使用相同请求头调用 `GET /v1/gateway/requests` 查询。成功响应为 `result.files[]`，每项只有 `provider`、`region`、`bucket` 和 `key`。在排队时可用 `POST /v1/gateway/requests/cancel` 取消，已经派发返回 409。

队列按 `model_key` 限制等待条数，`max_waiting` 默认 128。队列满返回 429，PostgreSQL 不可用返回 503。
派发时检查能力、探活和容量。暂时没有可用实例时，任务保持 `queued`，记录下次投递时间，消费者可以继续处理其他任务。`backend_timeout` 默认 120 秒，用于排队和上游等待。使用 Worker 协议的实例也用这份预算完成首次模型调用。截止时仍在队列的任务进入 `failed`，错误码为 `queue_timeout`。

## 实例管理

`POST /v1/instances/register` 使用管理凭据，提交以下字段。

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

商品图实例登记 `Vmind_cloth_gen_v1.0` 和 `cloth_gen`。切换前处理完旧模型键上的商品图任务并排空旧实例，再用新的 `instance_id` 登记。历史结果仍用原请求 ID 查询。

## 处理和恢复

Ops 使用管理凭据访问 `GET /v1/admin/requests` 和 `GET /v1/admin/requests/{request_id}`。列表支持请求 ID、Trace ID、模型或能力、实例、制品摘要、状态和时间过滤，`limit` 最大 100。详情返回对象引用和终态错误。两者都省略原始输入图片。实例查询分别返回 `reserved_requests` 和 `reserved_units`，健康与排空独立展示。

`requests` 保存原始业务 JSON、四态、截止时间、实际派发实例和制品、容量预留、后端任务及对象结果。受理时同一事务记录待投递时间，RabbitMQ 确认持久消息后才清除。消息只包含请求 ID，Broker 暂时不可用时由网关继续补投。消费者按 ID 加锁检查状态，提交归属和容量后再调用模型。已领取、已取消或已过期的任务收到重复消息时会直接确认。每个消费者预取一条，GPU 容量仍从已预留请求汇总。

使用 Worker 协议的实例通过 `POST {endpoint}/requests` 接收派发。模型端接收 `request_id`、`model_key`、`capability`、`input` 和 `metadata`。返回同一 `request_id`，状态为 `processing`、`succeeded` 或 `failed`。未受理的输入校验和鉴权错误使用 HTTP 400、413、415、401 或 403。已经受理的任务通过 `GET {endpoint}/requests/{request_id}` 查询，完成状态必须提供对象结果或结构化错误。

网关的状态为 `queued`、`processing`、`succeeded`、`failed`。副本退出后其他副本接管队列和状态查询。派发结果未知时保留槽位，查询原实例，禁止重发生成。确认终态后原子释放，迟到的确认不会改写已经对外失败的状态。

## 模型侧适配器

`worker_adapter/app.py` 在模型服务旁运行，复用 VMind-Image 的图片 multipart 路由和 LTX 的执行、任务查询及视频读取。适配器在本地目录保存受理和结果记录，上传成功后返回对象引用。用同一部署实例身份重启时保留该目录，每个目录只运行一个适配器进程。已发送图片请求在适配器自身中断且缺少后端终态时保持占槽，需核对模型执行事实。

部署配置提供 `VMIND_WORKER_STATE_DIR`、`VMIND_WORKER_BEARER`、`VMIND_WORKER_CAPABILITIES`、`VMIND_WORKER_INSTANCE_ID`、`VMIND_WORKER_ARTIFACT_DIGEST`、`VMIND_NATIVE_ORIGIN` 和 `VMIND_NATIVE_HEALTH_URL`。按模型服务需要配置 `VMIND_NATIVE_TOKEN`。对象存储使用 `VMIND_RESULT_PROVIDER`、`VMIND_RESULT_REGION`、`VMIND_RESULT_BUCKET` 和可选端点 `VMIND_RESULT_ENDPOINT`。OSS 配置端点和运行时 OSS 凭据。S3 使用 AWS 凭据链。

```sh
python -m pip install -r worker_adapter/requirements.txt
python -m uvicorn app:app --app-dir worker_adapter --host 127.0.0.1 --port 8001
```

## Go 网关直连

将原生地址加入 `native_backends`，再以相同的 `endpoint` 登记实例。健康地址和执行地址使用同一个协议、IP 和端口，端口加入 `backend_ports`。常规 VMind-Image 使用空对象。国内 FLUX 换装使用 `vton_edits` 选择 `/v1/images/edits`。LTX 的 `target_instance` 取自原生健康接口，并与登记的 GPU 分组对应。切换地址、协议或结果桶前先排空受影响的实例，再更新配置并重启网关。

```json
{
  "native_backends": {
    "http://10.0.1.10:9001": {"vton_edits": true},
    "http://10.0.1.11:9002": {"target_instance": "ltx-gpu0"}
  },
  "native_results": {
    "provider": "oss",
    "region": "cn-shenzhen",
    "bucket": "vmind-results",
    "endpoint": "https://oss-cn-shenzhen-internal.aliyuncs.com"
  }
}
```

VMind-Image 和 LTX 的原生鉴权分别使用 `MODEL_GATEWAY_NATIVE_IMAGE_*` 和 `MODEL_GATEWAY_NATIVE_VIDEO_*`。Basic Auth 注入 `USERNAME`、`PASSWORD`，`X-Token` 注入 `TOKEN`。OSS 从 `OSS_ACCESS_KEY_ID` 和 `OSS_ACCESS_KEY_SECRET` 取凭据。S3 使用 AWS 凭据链，`endpoint` 可省略。

直连视频通过 `/vton` 提交，按已记录的 `job_id` 查询 `/jobs/{job_id}`。网关把图片或视频写到 `outputs/<request_id>/`，通过 `result.files` 返回对象引用。图片响应体上限为 64 MiB，视频成品上限为 200 MiB。图片调用断线且终态未知时保留 GPU 槽位，等待核对原执行。

`native_execution_timeout` 默认 180 秒，从领取任务时起算，用于原生模型的首次调用和图片成品发布。上游等待超时后，已派发的任务继续等原生响应。网关记录终态时释放 GPU 槽位，调用方可以用原 `request_id` 查询结果。

## 网关启动

部署前，数据库侧根据 [Ent 模型定义](ent/schema) 准备 `model_gateway` schema、业务表和索引。已有数据库以自动提交方式执行 [RabbitMQ 迁移](deploy/migrations/rabbitmq-dispatch.sql)，并发索引需要独立提交。国内 ACK 使用 `wghub` 数据库。网关运行账号需要该 schema 的访问权限和业务表的读写权限。

国内 ACK 通过 workspace 的 `server_release.py release --service model-gateway --branch dev` 发布两个副本和内网 Service。镜像使用私有不可变仓库 `wg/model-gateway`。首次部署需要 [启动配置清单](deploy/ack-bootstrap.yaml) 和 Kubernetes Secret `model-gateway-runtime`。Nacos `wg.mirror.modelGateway` 保存监听、后端白名单、`rabbitmq_queue` 和容量设置。Secret 提供 `MODEL_GATEWAY_DATABASE_URL`、`MODEL_GATEWAY_RABBITMQ_URL` 和 `MODEL_GATEWAY_{API,ADMIN,BACKEND}_TOKEN`。

国内视频 Worker 设置 `VMIND_NATIVE_VIDEO_PROTOCOL=jobs` 和 `VMIND_NATIVE_VIDEO_TARGET`。目标实例名必须来自原生 `/healthz`，并与网关登记的 GPU 分组对应。提交原生 `/vton` 后持久化后端分配的 job_id，再查询 `/jobs/{job_id}`。原生服务未提供按外部 UUID 查询，受理响应丢失时保留槽位，待核实原执行。图片上游需要 Basic Auth 时，由模型部署提供 `VMIND_NATIVE_USERNAME` 和 `VMIND_NATIVE_PASSWORD`。

适配器镜像从仓库根目录使用 `worker_adapter/Dockerfile` 构建。部署端以稳定的内网 IP 和真实制品、GPU 归属登记实例，并持久挂载 `/state`。成品统一写入目标桶的 `outputs/<request_id>/`。

国内 ACK 通过 `workspace/deploy/server_release.py release --service model-gateway-worker --branch dev` 发布适配器。国内启用 `VMIND_WORKER_STATE_BACKEND=oss`，领取、受理和终态以独占写入保存到同一请求目录，Pod 重建后可继续回查。该 OSS 桶需保持版本控制未启用，确保条件写入拒绝覆盖。记录包含任务身份与状态，成品引用只包含实际媒体对象。

启动网关时必须设置 `XX_WG_REGION`。`CN` 用 `nacos_url` 连接 Nacos。`SG` 和 `US` 用 `appconfig_url` 连接 AppConfig Agent。在 `bootstrap.json` 中填入对应环境的地址。参见 [Nacos 引导示例](config/bootstrap.aliyun.example.json)和 [AppConfig 引导示例](config/bootstrap.aws.example.json)。

配置正文见 [gateway.example.json](config/gateway.example.json)。同一环境的副本共享 PG schema 和 RabbitMQ 队列，不同环境使用独立队列。网关会声明持久队列，需要配置、写入和读取权限。更换数据库或队列前先排空任务并重启。

```sh
go build -o bin/model-gateway ./cmd/model-gateway
export XX_WG_REGION=CN
bin/model-gateway -config /etc/model-gateway/bootstrap.json
```

`/_gateway/live` 检查进程，`/_gateway/ready` 检查请求账本。集成测试使用名称以 `_test` 结尾的独立 PostgreSQL 数据库和测试 RabbitMQ。

```sh
TEST_DATABASE_URL=<test-dsn> TEST_RABBITMQ_URL=<test-amqp-uri> go test -race ./...
```
