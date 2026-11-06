# modelGateway

Go 模型网关，一个服务可部署多个副本，共享 PostgreSQL。只使用 `model_instances` 和 `requests` 两张业务表。

模型由人工指定机器和 GPU 后启动，向统一入口注册。网关按 `model_key` 选择实例，在 PG 事务内预留 N 个容量单位并保存原始请求，随后调用后端。模型服务负责上传生成文件，网关只保存和返回 OSS/S3 对象位置。

## 请求路径

1. 模型服务注册实例，定期发送就绪状态和心跳。
2. 调用方生成 string UUID `request_id`，提交 `model_key`、`units` 和 `input`。
3. PG 按请求 ID 去重，原子占用实例容量并保存完整业务 JSON；容量不足返回 503，不排队。
4. 新请求返回 202，网关立即调用后端。调用方断开不取消已受理执行。
5. 后端同步返回结果，或返回异步任务 ID。网关后台按 `request_id` 查询后端状态。
6. 后端生成并上传完成后，网关在同一事务内写入终态和结果、释放 N。
7. 调用方通过 `GET /v1/requests/{request_id}` 获取状态和结果。重复提交返回已有记录，不再次派发或覆盖原始请求。

提交接口统一返回请求记录，尚未完成返回 202，已结束返回 200。它不透传长时间运行的 HTTP 响应流。后端是否同步完成，不改变上游的请求查询协议。

## 两张表

`model_instances`：`instance_id`、`model_key`、`endpoint`、`machine_id`、`gpu_devices`、`capacity_units`、`used_units`、`ready`、`draining`、`lease_expires_at`、创建和更新时间。

`requests`：`request_id`、`model_key`、`request_payload`、`instance_id`、`backend_task_id`、`reserved_units`、`status`、`result`、创建和更新时间。

完整 DDL 见 [migrations/001_init.sql](migrations/001_init.sql)。没有外键、结果过期时间或自动清理请求的任务。请求 JSONB 保留业务内容，不保证原 JSON 的空格、字段顺序。认证头不入库；输入应使用对象引用，不支持 multipart 文件上传。

`model_key` 包含版本含义。机器和 GPU 清单仅用于展示；多个模型共享 GPU 时，本机调度器仍须限制实际合计并发。实例 ID 使用规范的小写带连字符 UUID，每次模型进程重启使用新 ID。模型与 endpoint 不允许原地替换，旧实例记录保留。

容量单位由模型接口与可信调用方约定。`units` 是调用方声明的正整数，网关校验范围并检查总容量；它不估算显存或自动推导 GPU 成本。

## 运行

需要 Go 1.26.4 和 PostgreSQL。AWS 使用 AppConfig，阿里云使用 Nacos。运行配置从配置中心读取，应用不读取业务环境变量。

配置中心保存完整 JSON，字段见 [config/gateway.example.json](config/gateway.example.json)。数据库连接串、监听地址、各类令牌、后端网段和端口、调用超时、状态查询间隔、实例租约和请求体上限均支持热加载。

应用每 5 秒读取一次。AWS 通过本地 AppConfig Agent 获取缓存，实际生效时间还取决于 Agent 拉取和 AppConfig 发布进度。Nacos 使用 2.x OpenAPI 获取配置正文。读取、解析或应用失败时继续使用上一份配置，日志不输出配置正文或凭据。只保留必要的解析和既有鉴权、后端地址限制。

将对应引导文件挂载为 `/etc/model-gateway/bootstrap.json`：

- AWS 示例：[config/bootstrap.aws.example.json](config/bootstrap.aws.example.json)。AppConfig Agent 使用 ECS Task Role 或 EKS 工作负载身份访问 AWS。
- 阿里云示例：[config/bootstrap.aliyun.example.json](config/bootstrap.aliyun.example.json)。URL 中的 `tenant` 填 namespace ID。启用 Nacos 用户认证时，在受保护的引导文件中增加 `username` 和 `password`，应用自动登录和刷新令牌。

引导文件只保存中心入口和必要凭据，不提交实际引导文件。应用每轮也重新读取引导文件，中心地址和凭据变化无需重启。配置中心 JSON 中的时长使用 `120s`、`5s` 等格式。

更新后，新操作使用新配置。在途请求继续使用受理时的配置，完成后释放原数据库连接池。监听地址变化时先启用新监听，再排空旧监听。更换数据库连接串不会迁移历史请求和实例数据，目标数据库须已有对应数据及表结构。

```sh
go build -o bin/model-gateway ./cmd/model-gateway
# 显式初始化新 schema；服务启动不执行 DDL。
bin/model-gateway -config /etc/model-gateway/bootstrap.json -migrate migrations/001_init.sql
bin/model-gateway -config /etc/model-gateway/bootstrap.json
```

API、Worker 和 Admin 三种令牌分别对应可信调用方、模型注册和排空管理，必须各不相同且至少 24 字符。`backend_token` 用于调用模型服务，可留空。后端地址仍须使用允许网段中的字面 IP 和允许端口。应用调用配置中心及模型后端时均禁用环境 HTTP 代理。

接口依据：[AWS AppConfig Agent](https://docs.aws.amazon.com/appconfig/latest/userguide/appconfig-integration-containers-agent-retrieving-data.html)、[Nacos OpenAPI 认证](https://nacos.io/en/docs/v2/guide/user/auth/)。

`GET /health/live` 检查进程，`GET /health/ready` 检查 PG。就绪不代表所有模型都已注册。

## 模型注册接口

以下接口使用 Worker 令牌，格式为 `Authorization: Bearer <worker_token>`。

`POST /v1/instances/register`：

```json
{
  "instance_id": "b48601dc-121e-4e79-9c46-b0d50ec5fd38",
  "model_key": "vton_v3",
  "endpoint": "http://10.0.1.10:8001",
  "machine_id": "machine-a",
  "gpu_devices": [0, 1],
  "capacity_units": 4,
  "ready": true
}
```

`POST /v1/instances/{instance_id}/heartbeat`：`{"ready":true}`。建议每 10 秒发送一次，服务器使用 PG 时间续租，默认租约为 30 秒。重复注册和心跳不能清零占用或解除排空。相同模型和地址仍有活跃实例或未完成占用时，不能用新实例 ID 重新注册来绕过容量限制。

`POST /v1/instances/{instance_id}/drain`：`{"draining":true}`，使用 Admin 令牌。排空只停止新请求，不取消在途任务。

## 请求接口

使用 `Authorization: Bearer <api_token>`。本版本面向互信的服务调用方，不实现多租户数据隔离。

`POST /v1/requests`：

```json
{
  "request_id": "63565d4a-45b1-419e-bfbf-4ecb78cd34d4",
  "model_key": "vton_v3",
  "units": 2,
  "input": {
    "prompt": "换成红色上衣",
    "image": {
      "provider": "s3",
      "region": "us-east-2",
      "bucket": "input-bucket",
      "key": "inputs/person.png"
    }
  }
}
```

同一个请求 ID 的内容必须保持不变。网关按 ID 返回第一次提交的记录，不额外计算输入指纹。没有容量时不保存请求，可以复用同一个 ID 稍后重试。

`GET /v1/requests/{request_id}` 返回记录，包括 `request_payload`、`status`、`reserved_units` 和 `result`。

## 模型后端协议

模型实例必须提供以下接口。本仓库定义新协议，不假定现有图片或视频服务已经支持。

- `POST {endpoint}/requests`：接收完整原始业务 JSON，必须按 `request_id` 保留可查询状态。
- `GET {endpoint}/requests/{request_id}`：返回同一任务的最新状态，包括受理响应丢失后的查询。

成功响应为 HTTP 2xx：

```json
{
  "status": "succeeded",
  "result": {
    "files": [
      {
        "provider": "s3",
        "region": "us-east-2",
        "bucket": "output-bucket",
        "key": "outputs/request-id/video.mp4"
      }
    ]
  }
}
```

阿里云使用 `provider: "oss"`。每个文件只返回 provider、region、bucket、key，不返回 URL。多个结果使用 files 数组。后端必须在上传成功后才返回 succeeded。

异步受理返回 `{"status":"running","backend_task_id":"task-1"}`，保留容量。确认执行结束且失败时，返回 HTTP 2xx 和 `{"status":"failed","result":{"error_code":"generation_failed","message":"原因"}}`。

非 2xx、无效响应、超时和读取断连均标记 unknown，不自动重试 POST。HTTP 404 也不能单独证明后端未执行。连接建立前的明确 dial 失败可记录 failed 并释放。

## 故障处理

没有全局单活锁。相同请求 ID 的短事务通过 PG 事务锁串行去重，实例行锁保护容量预留；锁不跨 HTTP 调用持有。重复完成通知只会释放一次容量。

状态为 reserved、dispatching、running、succeeded、failed、unknown。只有已确认的 succeeded / failed 释放容量。unknown 继续占用并查询原后端。

后台默认每 5 秒处理一批状态查询，各副本用 PG 行锁认领。状态查询超时 3 秒；提交后端默认超时 120 秒。未派发的 reserved 超过 30 秒可原子失败并释放；dispatching 超过调用预算后按请求 ID 核对。后台查询是状态恢复，不重新派发生成请求。

后端不支持按请求 ID 查询时，无法自动解决结果未知。实例丢失、文件丢失、共享 GPU 的直接调用，也不是网关 PG 计数能够自动修复的。本版本不提供强制清零接口。

## 本地验证

```sh
# 必须使用独立测试数据库，名称以 _test 结尾。
TEST_DATABASE_URL=postgresql://postgres:local-test-only@127.0.0.1:55439/model_gateway_test go test -race ./... -count=1
go vet ./...
```

`TEST_DATABASE_URL` 仅供测试使用。测试会清空该测试库中这两张表，配置热加载测试还会创建并清理独立临时测试库。CI 使用临时 PG 18；没有 TEST_DATABASE_URL 时集成测试会跳过，不代表 PG 验收通过。

此仓库本轮只实现和本地验证新网关。旧服务、线上数据库和流量入口均不改动。旧数据转换、真实模型协议适配及部署必须在后续单独验收。
