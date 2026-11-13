# 通用图像与视频生成

图像和视频使用两套独立协议。`model` 是实例登记的 `model_key`，不使用专家名，也不根据模型名称推断业务。新增遵守协议的模型只需部署和登记，无需修改网关的模型列表或业务分支。

## 调用接口

使用 `Authorization: Bearer <api_token>`。可选的 UUID `Idempotency-Key` 用于复用同一任务；省略时网关生成 UUID，通过响应头 `X-Gateway-Request-ID` 返回。相同键和相同模型、输入复用原结果，改变模型或输入返回 409。查询、下载也需要 API 凭据。

### 图像

`POST /v1/images/generations`，`Content-Type: application/json`：

```json
{
  "model": "my-image-model-v2",
  "prompt": "A coastal village at sunrise",
  "images": [],
  "n": 2,
  "size": "1536x1024",
  "parameters": {
    "seed": 42,
    "steps": 28,
    "guidance": 3.5
  }
}
```

- `prompt` 和 `images` 至少提供一个。没有参考图时为文生图；参考图按数组顺序传入，网关不赋予人物、衣服等业务角色，也不限制为特定业务所需的图片数量。
- 每个参考图为 `{"mime_type":"image/png","data":"<base64>"}`，支持 PNG、JPEG、WebP。实际能接收几张图，由选择的模型决定。
- `n` 可选，提供时须为正整数；`size` 可选，网关不固定画布；支持的数量和尺寸由模型校验。
- `response_format` 可省略或为 `b64_json`。当前不支持 URL 结果。
- `parameters` 是可选 JSON 对象，数值、布尔、数组和嵌套对象保留类型，原样传给模型；不展开或覆盖外层 `model`、`prompt` 等字段。
- 可选 `user` 与 `X-Trace-ID` 用于追踪，不影响任务幂等身份。

图片完成后返回 HTTP 200：

```json
{"model":"my-image-model-v2","created":1794484800,"data":[{"b64_json":"<image-1-base64>"},{"b64_json":"<image-2-base64>"}]}
```

`POST /v1/images/edits` 是同一图像协议的 multipart 上传入口：文本字段为 `model`、`prompt`、`n`、`size`、`response_format`、`user`、`parameters`；参考图使用重复的 `image` 或 `image[]` 文件字段，至少一张。`parameters` 表单值为 JSON 对象字符串。网关将其转换为上述 JSON 协议调用模型。

### 视频

`POST /v1/videos`，`Content-Type: application/json`：

```json
{
  "model": "my-video-model-v3",
  "prompt": "The camera slowly follows the subject",
  "images": [{"mime_type":"image/png","data":"<base64>"}],
  "seconds": 5,
  "size": "1280x720",
  "parameters": {"fps":24,"seed":42,"audio":true}
}
```

`prompt` 和 `images` 至少提供一个。文生视频不需要首帧；参考图按顺序传递，具体用途由模型解释。`seconds` 可选，提供时须为正数；`size`、`parameters`、`user` 可选。视频不接受图像专用的 `n` 和 `response_format`。

也支持 multipart：文本字段为 `model`、`prompt`、`seconds`、`size`、`parameters`、`user`，图片使用可重复的 `input_reference` 文件字段。无参考图的 multipart 请求也可提交。

提交后返回 HTTP 200，无需等待生成：

```json
{"id":"<gateway-request-uuid>","object":"video","model":"my-video-model-v3","status":"queued","created_at":1794484800}
```

- `GET /v1/videos/{id}` 查询任务，状态为 `queued`、`in_progress`、`completed` 或 `failed`；失败时包含 `error.code`、`error.message`。
- `GET /v1/videos/{id}/content` 下载已完成的 MP4。未完成返回 409。
- 网关 ID 与模型端 job ID 分开保存，查询与下载始终使用该任务实际派发的实例地址。

## 模型服务端契约

登记 `protocol: "image_generation"` 或 `protocol: "video_generation"`。同一模型键固定一种协议，可登记多个执行实例；另一类接口会在入队前拒绝该模型。旧业务协议不能通过通用入口调用，不会被自动翻译成业务请求。

模型服务直接实现下面的接口，路径相对于登记的 `endpoint`（支持 `/pool/gpu0` 等基础路径，不带尾部 `/`）：

| 协议 | 模型服务接口 | 返回值 |
|---|---|---|
| `image_generation` | `POST /v1/images/generations` | 同步 JSON `model`、`data[].b64_json` |
| `video_generation` | `POST /v1/videos` | JSON `model`、`id`、`status` |
| `video_generation` | `GET /v1/videos/{id}` | JSON `model`、`id`、`status`，状态同上 |
| `video_generation` | `GET /v1/videos/{id}/content` | MP4 字节 |

POST 请求使用相同的 JSON 输入契约，`model` 保持登记键。模型服务负责将通用输入转换为自己的推理调用，并校验实际支持的参数、尺寸、参考图数量。网关不会生成业务提示词或选择权重。未知参数应由模型返回 400/422，避免静默忽略。

模型请求携带稳定的 `Idempotency-Key` 和 `X-Gateway-Request-ID`，重试始终使用同一 UUID。后端必须按该键幂等处理，包括返回相同视频 job ID，避免断连重试重复生成。网关不改变模型特定参数。图像请求省略 `response_format` 时，后端请求补为 `b64_json`。

模型端返回的图像 JSON，以及视频完成查询的 JSON，可额外携带现有 `generation` 回执，网关继续保存。模型视频 ID 允许 1–128 位字母、数字、下划线和连字符，首位为字母或数字。

`health_url` 必须属于同一登记地址，返回 `{"ready":true,"models":["登记的模型键"]}`，可携带 `artifact_digest`。通用模型的探活、提交、查询及下载使用 `MODEL_GATEWAY_BACKEND_TOKEN`（配置后发送 Bearer 凭据），与旧原生业务接口的认证配置分开。

## 登记与运行配置

先按现有资源组接口建立 GPU 资源组，再登记，例如：

```json
{
  "instance_id":"image-v2-gpu0",
  "model_key":"my-image-model-v2",
  "protocol":"image_generation",
  "endpoint":"http://10.0.0.8:8001",
  "health_url":"http://10.0.0.8:8001/health",
  "resource_group":"host-a/gpu0",
  "capacity_units":1,
  "machine_id":"host-a",
  "gpu_devices":[0]
}
```

通过管理员凭据提交到 `POST /v1/instances/register`。通用模型不需要 `native_models` 或 `native_backends` 中的逐模型绑定；协议来自登记，执行地址来自选中的实例。仍需配置后端网络/端口白名单，以及 `native_results` 的 OSS/S3 存储与凭据，网关负责保存生成结果并返回或提供下载。

复用现有 PostgreSQL 账本、RabbitMQ、资源组容量与重试机制。`backend_timeout` 控制排队和调用方等待，首次生成调用使用 `native_execution_timeout`。图像 HTTP 响应等待超时不取消已入队任务，可通过原任务查询接口用同一个请求 UUID 查询；重发相同输入和幂等键也不会创建新任务。

现有大小限制继续生效：默认请求体 64 MiB、图像模型响应 64 MiB、图像下载响应解码后的总量不超过 64 MiB、视频成品 200 MiB。multipart 最多 16 个字段，各文本字段最多 4096 字节；较大的提示词或参数对象可使用 JSON 入口。

现有 `/v1/gateway/requests/{model_key}` 及旧业务协议保留兼容，也可提交通用协议的 `input`，继续返回原任务/对象引用格式。新通用接口不提供 `expert`、业务字段或生产/实验凭据间的生成参数差异。现有模型服务需要实现以上契约并用通用协议登记，不能只更改登记标签而继续提供旧业务接口。没有数据库结构变更；旧网关版本不认识新协议，滚动升级时应先升级全部网关/Worker，再登记通用模型。


## 与 VMind-Image 对接

VMind-Image 通过 Worker 的 `VMIND_IMAGE_MODELS` 绑定模型键到驻留权重，并通过共享入口的 `IMAGE_MODEL_ROUTES` 选择实际 Worker。`health_url` 使用 `<endpoint>/v1/images/models/<model_key>/health`；网关要求 ready 且 models 列表包含登记键。生成结果中的 model 必须与任务模型一致，缺失或不匹配返回 `model_identity_mismatch`，避免旧服务忽略 model 后返回其他权重的结果。

断连、超时等未知结果的通用请求重试保留最初实例、执行地址和制品身份，不会切到另一个 Worker 绕过其幂等状态。只有明确 `worker_busy` 且 `execution_started=false` 才允许重新选择实例。Worker 的持久幂等目录必须随部署保留。

现有业务模型键的协议固定在历史登记中；兼容接入应使用新的通用模型键和 instance_id，共享原 resource_group。不能直接将相同 model_key 重新登记为另一协议。VMind-Image 的 `docs/generic-image-generation.md` 和 `deploy/unified/generic-models.env.example` 提供完整配置与 CPU 契约测试说明。
