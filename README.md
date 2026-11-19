# modelGateway

一个模型网关可运行多个 Go 副本，共享 PostgreSQL 和 RabbitMQ。Hertz 处理 HTTP，Ent 定义 `resource_groups`、`model_instances` 和 `requests`。每个副本启动两个 RabbitMQ 消费者。模型执行端确认终态并上传成品，ModelHub 使用 OSS/S3 对象引用读取。

## 通用图像和视频生成

- 图像：`POST /v1/images/generations`，JSON 输入；`POST /v1/images/edits` 为参考图 multipart 上传入口。
- 视频：`POST /v1/videos`，独立的视频输入协议；通过 `GET /v1/videos/{id}` 查询、`GET /v1/videos/{id}/content` 下载。
- `model` 直接选择登记模型，`prompt`、参考图和生成参数传给模型服务。网关不提供专家或业务能力，也不绑定模型名、画布或业务提示词。
- 模型登记为 `image_generation` / `video_generation` 后，Worker 直接调用对应的通用模型接口。新增兼容模型只需登记，无需修改路由代码。

请求示例、后端契约、注册和迁移要求见 [通用生成接口](docs/GENERATION_API.md)。

## 兼容调用协议

统一调用入口为 `POST /v1/gateway/requests/{model_key}`，承接图片、视频、对话和视觉模型。请求使用 `Authorization: Bearer <api_token>`、`Content-Type: application/json` 和 UUID `Idempotency-Key`。UUID 同时是请求表主键和模型端查询 ID。

请求体包含 `input` 和可选的 `metadata`。图片按顺序放入 `input.images`，每项为 `mime_type` 和 base64 `data`。同一键和相同业务输入复用原任务，改模型或业务输入返回 409。追踪字段变化不改变任务身份。

### 模型目录和 JSON Schema

调用前用 API 凭据读取 `GET /v1/models?category=image_generation`（category 可省略），目录按模型键去重，不返回实例数量。`GET /v1/models/{model_key}` 返回 `model_key`、`protocol`、`category`、`description`、`response_mode` 和完整 `input_schema`。

`model_instances.input_schema` 是 JSONB，保存完整 JSON Schema 2020-12。定义包含字段、类型、必填项、图片顺序和数量；运行时由 JSON Schema 库校验，再由已有适配器编码并派发。`protocol` 继续选择内部编码与执行方式，实例 `endpoint` 继续决定执行地址，调用方不选择内部路径。

媒体 input 直接使用模型 Schema 的字段，例如 VTON 的 `images`/`prompt`、拆衣的 `garment_name`/`bbox`/`parse_labels`、体模的 `gender`。不再接受 `text`/`model_traits` 兼容信封；protobuf 转换由 ModelHub 完成。Decision Schema 约束恰好一张内联图片、消息内容类型、预设 task 与用户问题互斥，以及 options 仅供自定义问题使用；执行适配器只解码图片、识别真实格式并编码模型表单。

字段不符合 Schema 时返回 HTTP 400、`error.code=invalid_input`。`error.message` 只提供输入字段的 JSON Pointer 和 Schema 规则关键字，不回显图片或其他输入值；根对象的 Pointer 为空字符串。客户端按 HTTP 状态和 `error.code` 判断结果，message 用于诊断。

同模型多个实例保留重复 schema。管理凭据调用 `PUT /v1/admin/models/{model_key}/schema`，请求 `{"input_schema":{...}}`，在一个事务里更新该模型全部登记；新副本继承已有定义，不按协议重置。业务协议首次登记可提交 input_schema，省略时使用协议种子；`image_generation` / `video_generation` 首次登记必须显式提交模型 Schema。定义只允许文档内部 `$ref`，不加载远程或本机文件。

支持透明输出的环境应先更新网关和视频执行端，再执行 [穿搭视频字段迁移](deploy/migrations/outfit-video-transparent-background.sql)，为已有模型登记增加该字段。

在所有可调度的 Qwen 试衣和体模执行端都支持 RGBA 后，可在对应环境执行 [图片透明字段迁移](deploy/migrations/qwen-business-transparent-background.sql)。两种图片业务的 `input.transparent_background` 为 true 时返回透明背景 PNG，省略或为 false 时返回 RGB PNG。

**发布前先执行 [input_schema 迁移](deploy/migrations/model-input-schema.sql)**，回填已有 13 种协议的初始契约；已有自定义定义不会被覆盖。种子用于迁移和首次登记，运行时缺少定义返回 503。`x-category` 提供目录类别；`x-experimental-properties` 只对实验凭据展开，对外 schema 与当前凭据实际校验规则一致。

对话 input 使用 OpenAI-compatible JSON（模型由路径指定），响应仍是原生 JSON/SSE；视觉 input 使用 JSON 和 Base64 内联图片，由 Gateway 编码成上游 multipart/JSON，响应保持原生 JSON/图片。OpenAI-compatible 种子保留扩展字段，具体模型的参数支持范围可由管理员在模型定义中收紧；schema 通过不替代上游模型的语义校验。媒体生成仍使用下面的任务结果格式。生成整合阶段继续保留 `/v1/chat/completions`、`/v1/vision/...` 和 `/v1/decision/predict`，沿用原鉴权、上传限制、实例路由与账本。视觉/对话调用方的迁移及旧入口删除另行发布。

每个 `model_key` 固定对应一种业务。输入校验、实例匹配和后端路由使用同一个键。业务含义变化时使用新键。

接入方按以下顺序适配：

1. 使用目标环境的 API Bearer Token 调用 `GET /v1/models` 选择模型，再用 `GET /v1/models/{model_key}` 读取该环境的完整定义。
2. 按 `input_schema` 构造 JSON，携带 UUID `Idempotency-Key` 调用 `POST /v1/gateway/requests/{model_key}`，请求体为 `{"input":{...}}`。图片字段、顺序和数量以查询到的定义为准。
3. 读取定义的 `response_mode`：`direct` 返回模型原生 JSON/图片，聊天 `stream=true` 时返回 SSE；`task` 使用图片同步任务结果或视频 202+查询。任务查询使用 `GET /v1/gateway/requests` 和原 UUID，结果在 `result.files`，文件可通过带 API 凭据的 `GET /v1/admin/requests/{request_id}/media?kind=output&index=0` 读取。
4. 联调环境先完成本版本部署、Schema 迁移和实例登记；保留旧固定推理入口，并兼容生产 ModelHub 的图片 `text` / `model_traits` 信封：只展开已知四种业务图像协议，转换后仍执行同一份数据库 Schema 校验。新调用方应发送规范字段，不混合新旧格式。请求限制针对编码后的整个 JSON，Base64 约为原图的 4/3；实际预算读取环境配置，不能直接按原图字节数套用请求上限。

校验库固定使用 [无用字符串复制修复](https://github.com/Fragmentaim/jsonschema/commit/32495a5894a5) 的依赖版本；补丁只在启用内容检查时构造字节，不改变 Schema 规则。上游合入同等修复后可以移除 `go.mod` 的替换。

| 模型键 | 业务 | 输入 |
|---|---|---|
| `Vmind_wardrobe_v1.0` | 人物拆衣 | 人物图、衣物名称和衣物框或人体解析输入。 |
| `Vmind_cloth_gen_v1.0` | 商品图 | 单件原图、衣物名称和可选角色。 |
| `Vmind_outfit_v1.0` | 试衣 | 人物图、衣物图和提示词。 |
| `Vmind_bodymodel_v1.0` | 体模 | 人物图与性别。 |
| `Vmind_outfit_video_v1.0` | 试衣视频 | 首帧、提示词和可选动作参数。可选布尔字段 `transparent_background` 为 true 时返回透明 WebM，省略或 false 返回 MP4。 |

实验凭据 `MODEL_GATEWAY_EXPERIMENTAL_TOKEN` 可选，未设置时关闭。它具备 API 凭据的全部权限（含历史请求及输入输出读取），不能访问其他管理接口；受理时在上表业务字段之外，额外放行以下生成参数，原样转发给模型服务。生产凭据仍按服务端默认值执行，传入这些字段返回 400。

| 模型键 | 实验参数 |
|---|---|
| `Vmind_wardrobe_v1.0` | `prompt`、`use_pe` |
| `Vmind_bodymodel_v1.0` | `prompt` |
| `Vmind_outfit_v1.0` | `use_pe` |
| `Vmind_outfit_video_v1.0` | `model`、`duration`、`resolution`、`fps`、`audio` |

参数值均为字符串。实验请求的参数保存在请求记录中，可在管理端预览。

图片持久入队后同步等待结果。视频持久入队后立即返回 202 和 `request_id`、`state`。使用相同请求头调用 `GET /v1/gateway/requests` 查询。成功响应为 `result.files[]`，每项只有 `provider`、`region`、`bucket` 和 `key`。在排队时可用 `POST /v1/gateway/requests/cancel` 取消，已经派发返回 409。

队列按 `model_key` 限制等待条数，`max_waiting` 默认 128。队列满返回 429，PostgreSQL 不可用返回 503。`units_by_model_key` 配置每个模型键的占用单位，未配置的键按 1 个单位计算。受理时把占用单位写入请求账本。

派发时按模型键查找健康且未暂停的实例，再检查实例和共享资源组的容量。暂时没有可用实例时，任务保持 `queued`，记录下次投递时间。`backend_timeout` 默认 600 秒，用于排队和上游等待。使用 Worker 协议的实例也用这份预算完成首次模型调用。截止时仍在队列的任务进入 `failed`，错误码为 `queue_timeout`。

### G6 视觉推理

在目标环境的网关配置中设置 `vision_addr`（私网 IP 和端口），例如美东路演 G6 的 `172.31.43.7:18280`。视觉模型同样通过 `POST /v1/gateway/requests/{model_key}` 调用，携带 API Bearer Token、UUID `Idempotency-Key` 和 JSON `input`。字段定义从 `GET /v1/models/{model_key}` 的 `input_schema` 读取。

图片以 Base64 放在 input 的 `image`、`file`、`image_a` / `image_b` 或 `base64` 等模型定义字段中；不接收图片 URL 或对象存储路径。Gateway 按登记协议编码原生 JSON/multipart，由登记实例的 endpoint 和容量决定派发，完成后返回原生结果。同步请求也记录到持久账本并占槽，但不经 RabbitMQ 生成任务队列。

管理接口 `GET /v1/vision/health` 继续用于视觉服务探测。

## Embedding

`embedding` 协议使用统一入口 `POST /v1/gateway/requests/{model_key}`，将 `input` 原样发送到已领取实例的 `/v1/embed`。同步返回原生 `embeddings`、`dim`、`count`、`latency_ms`，复用同步账本、物理 GPU 占槽和终态释放。

文本示例：`{"input":{"inputs":["蓝色衬衫"],"dim":1024,"normalize":true}}`。图片项使用 `{"image":"data:image/png;base64,..."}`，可同时附带 `text`；不接受文件路径或远端图片 URL。首次登记继承 `embedding` Schema，管理员按模型原生维度设置 `dim.maximum`。

每条实例登记必须指向固定 GPU 副本，不能把负载均衡入口伪装成单卡实例。`health_url` 必须为 `endpoint + "/health"`，HTTP 200 且 JSON `status` 为 `ok` 才就绪；地址仍受网关出站白名单约束。相同物理卡上的不同 Embedding 模型复用已有资源组。

2B 与 8B 使用不同模型键；即使都返回 1024 维，也必须分别维护索引。

## 实例管理

先在 Ops 的“GPU 资源组”页面建组，或用管理凭据调用 `POST /v1/resource-groups`。请求体示例为 `{"id":"g7e/gpu0","machine_id":"g7e-a","gpu_device":0,"capacity_units":1}`。同一机器上的同一物理卡只能建一个组。

用 `GET /v1/resource-groups` 查询容量、占用和实例数。用 `PATCH /v1/resource-groups` 提交 `id` 和 `capacity_units` 调整组上限。调低容量后，已派发任务继续执行，新任务等待空位。

`POST /v1/instances/register` 使用管理凭据，提交以下字段。

```json
{
  "instance_id": "g7e-vton-a",
  "model_key": "Vmind_outfit_v1.0",
  "endpoint": "http://10.0.1.10:8001",
  "health_url": "http://10.0.1.10:8001/health/ready",
  "artifact_digest": "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "resource_group": "g7e/gpu0",
  "capacity_units": 1,
  "machine_id": "g7e-a",
  "gpu_devices": [0]
}
```

登记时使用已有组标识和实例容量。机器和卡号必须与资源组匹配。网关忽略旧客户端传入的 `group_capacity_units`，组上限统一在资源组管理接口修改。迁移后未绑定物理卡的历史组允许同模型、同地址的部署重新登记。

网关主动探测。探针返回 `ready: true` 后实例可以接单。使用管理凭据调用 `GET /v1/instances` 查询和 `POST /v1/instances/{id}/drain` 设置 `{"draining":true}`。部署身份变化时先排空旧实例，再登记新实例。

实例列表用 `clearable` 标明可清除的登记。使用管理凭据调用 `DELETE /v1/instances/{id}`，网关在同一事务里确认实例已暂停、未就绪且没有占槽请求。状态不满足时返回 409。
清除只写入 `deleted_at`，实例从列表移除。历史请求仍可按原实例 ID 查询，新部署使用新的实例 ID。
请求统计为模型返回 `retired`：仅全部登记已软删除、无排队/执行及保留槽位时为 true。暂停、不健康、从未登记和部分实例退役不会被标记。统计仍返回完整历史，由概览默认隐藏明确退役的模型；可切换查看已停用历史。清理验收登记前核对部署已停止或回退、容器已停且无未结束请求，按已确认的具体实例 ID 清理，禁止按 `verify_` 前缀或不健康状态批量清理。

商品图实例登记 `Vmind_cloth_gen_v1.0`。旧版商品图和拆衣曾共用 `Vmind_wardrobe_v1.0`，已有数据库按下方迁移步骤切换。

## 处理和恢复

API 凭据（含独立 Token 和实验凭据）与管理凭据均可访问 `GET /v1/admin/requests`、`GET /v1/admin/requests/{request_id}` 及其媒体读取接口，可读取当前环境全部调用方的历史输入输出；接口路径保持不变。统计及其他管理接口仍只接受管理凭据。列表的 `model` 参数精确匹配 `model_key`。列表也支持请求 ID、Trace ID、实例、制品摘要、状态和时间过滤，`limit` 最大 100。详情返回对象引用、终态错误，以及去掉图片字节后的业务参数和媒体清单。单张输入或成品通过 `GET /v1/admin/requests/{request_id}/media?kind=input|output&index=0` 读取。列表省略原始输入图片和完整结果，但带出受理时记在 `extra.input_media` 的 `inputs` 与按结果对象列出的 `outputs`，每项含 `index`、`role`、`mime_type`，输入图另含读得出的 `width`、`height`。上线前执行 [extra 迁移](deploy/migrations/request-extra.sql)，仅新增可空的 `extra jsonb`，不回填历史数据。没有 `extra.input_media` 的请求在列表里省略 `inputs`，调用方需读详情获取输入清单；这也包括加列后仍由旧副本受理的请求。`outputs` 始终从结果生成，没有成品时返回空数组。`GET /v1/admin/request-stats?from=&to=` 按 `model_key` 汇总时间窗内的请求量、成功与失败数、成功请求总耗时和排队耗时的 P50/P95，以及 24 个等宽时间桶的请求量和失败量。`queued`、`processing` 是当前积压，不受时间窗限制。同一响应按错误码、模型和实例分组返回最多 20 组失败，每组附带最近一次的请求 ID 和错误信息。时间窗默认最近 24 小时，可在 1 小时到 7 天之间。实例查询分别返回 `reserved_requests` 和 `reserved_units`，健康与排空独立展示。

`requests` 保存原始业务 JSON、四态、截止时间、实际派发实例和制品、容量预留、后端任务及对象结果。受理时同一事务记录待投递时间，RabbitMQ 确认持久消息后才清除。消息只包含请求 ID，Broker 暂时不可用时由网关继续补投。消费者按 ID 加锁检查状态，提交归属和容量后再调用模型。已领取、已取消或已过期的任务收到重复消息时会直接确认。每个消费者预取一条，GPU 容量仍从已预留请求汇总。

使用 Worker 协议的实例通过 `POST {endpoint}/requests` 接收派发。模型端接收 `request_id`、`model_key`、`input` 和可选的 `metadata`。返回同一 `request_id`，状态为 `processing`、`succeeded` 或 `failed`。未受理的输入校验和鉴权错误使用 HTTP 400、413、415、401 或 403。已经受理的任务通过 `GET {endpoint}/requests/{request_id}` 查询，完成状态必须提供对象结果或结构化错误。

网关的状态为 `queued`、`processing`、`succeeded`、`failed`。首次模型调用遇到 Worker 忙、断线、执行超时、5xx 或无法读取的响应时，归还本次网关槽位并记录下一次 MQ 投递，约一秒后重试。重试沿用原请求 ID、输入和总截止时间，到期后进入失败状态。参数、鉴权和其他明确的 4xx 拒绝直接失败。每轮派发使用独立的领取标识，迟到的旧结果不能覆盖新一轮。已经保存后端任务 ID 的异步任务继续查询原任务。

回队重试允许后端重复执行。断线不能证明 Worker 尚未开始，同一请求 ID 也不代表 Worker 已实现去重，实际 GPU 并发由 Worker 控制。

模型返回 400、413、415 或 422 的 JSON 校验错误时，网关保留 `error.field` 和 `error.message`（兼容旧接口的字符串 `detail`），合并到持久错误消息中，例如 `prompt: Must be at most 32000 characters (received 32001)`。调用结果、历史查询和 Ops 详情显示同一消息。错误码仍为 `model_rejected`，校验拒绝不重试并释放槽位；不可解析、超长或非 JSON 响应继续使用通用错误，鉴权与 5xx 诊断不对外透传。

## 模型侧适配器

`worker_adapter/app.py` 在模型服务旁运行，复用 VMind-Image 的图片 multipart 路由和 LTX 的执行、任务查询及视频读取。适配器在本地目录保存受理和结果记录，上传成功后返回对象引用。用同一部署实例身份重启时保留该目录，每个目录只运行一个适配器进程。已发送图片请求在适配器自身中断且缺少后端终态时保持占槽，需核对模型执行事实。

部署配置提供 `VMIND_WORKER_STATE_DIR`、`VMIND_WORKER_BEARER`、`VMIND_WORKER_MODEL_KEYS`、`VMIND_WORKER_INSTANCE_ID`、`VMIND_WORKER_ARTIFACT_DIGEST`、`VMIND_NATIVE_ORIGIN` 和 `VMIND_NATIVE_HEALTH_URL`。`VMIND_WORKER_MODEL_KEYS` 使用逗号分隔的完整模型键。按模型服务需要配置 `VMIND_NATIVE_TOKEN`。对象存储使用 `VMIND_RESULT_PROVIDER`、`VMIND_RESULT_REGION`、`VMIND_RESULT_BUCKET` 和可选端点 `VMIND_RESULT_ENDPOINT`。OSS 配置端点和运行时 OSS 凭据。S3 使用 AWS 凭据链。

```sh
python -m pip install -r worker_adapter/requirements.txt
python -m uvicorn app:app --app-dir worker_adapter --host 127.0.0.1 --port 8001
```

## Go 网关直连

在 Ops 登记实例的 `endpoint` 和 `health_url`，Go 网关直接调用该地址。协议通过 `native_models` 按模型类型配置一次；新增 GPU 不需要增加 ACK 适配器、服务端口或地址映射。

私网 IP 必须位于 `backend_cidrs`；公网地址必须使用 HTTPS，域名或公网 IP 必须匹配 `backend_public_hosts`，端口必须位于 `backend_ports`。公网 DNS 每次建连都校验全部解析结果，拒绝回环、私网、链路本地及其他特殊用途地址；连接使用已校验的 IP，TLS 仍验证登记域名。关闭代理和重定向。可以登记固定端口或带路径前缀的地址。

```json
{
  "backend_cidrs": ["10.96.0.0/16"],
  "backend_ports": ["8001", "8002", "443", "8443"],
  "backend_public_hosts": ["*.seetacloud.com"],
  "native_models": {
    "Vmind_outfit_v1.0": {},
    "Vmind_bodymodel_v1.0": {},
    "Vmind_outfit_video_v1.0": {"video": true}
  },
  "native_results": {
    "provider": "oss",
    "region": "cn-shenzhen",
    "bucket": "vmind-results",
    "endpoint": "https://oss-cn-shenzhen-internal.aliyuncs.com"
  }
}
```

例如图片实例登记 `https://node.westd.seetacloud.com:8443/vworld/gpu3`，健康地址为该地址加 `/health`。视频实例可以登记 `https://node.bjb2.seetacloud.com:8443/gpu0`，健康地址为该地址加 `/healthz`。专用单卡端口也可直接登记根地址，无需强制 `/gpuN` 路径。视频探针必须报告一个 worker，卡号与登记的 `gpu_devices` 一致；路径带 `/gpuN` 时也校验路径中的编号。

Outfit 与 BodyModel 若使用同一张物理卡，必须选择同一个资源组。组容量为 1 时，两种模型合计只能占用一个执行槽位。增加 GPU 时先在 Ops 创建对应物理卡的资源组，再登记该卡提供的模型实例；组和实例的增加不改变 ACK 部署。

`native_backends` 的旧地址绑定仍兼容，且优先于 `native_models`，用于渐进迁移。新部署使用 `native_models`。修改协议或结果桶需排空并重启网关；新增允许范围内的 endpoint 只需 Ops 登记。公网域名允许规则只配置可信模型提供方；Basic Auth 和 Token 仍由网关 Secret 注入，不写入 endpoint。国内旧 FLUX 换装可在对应模型配置中设置 `vton_edits: true`；Qwen-Image 使用空对象。

CN 的配置、迁移验收和回滚步骤见 [CN endpoint 部署](deploy/CN-ENDPOINTS.md)。

VMind-Image 和 LTX 的原生鉴权分别使用 `MODEL_GATEWAY_NATIVE_IMAGE_*` 和 `MODEL_GATEWAY_NATIVE_VIDEO_*`。Basic Auth 注入 `USERNAME`、`PASSWORD`，`X-Token` 注入 `TOKEN`。OSS 从 `OSS_ACCESS_KEY_ID` 和 `OSS_ACCESS_KEY_SECRET` 取凭据。S3 使用 AWS 凭据链，`endpoint` 可省略。

直连视频在登记的单卡地址下调用 `/vton`、`/jobs/{job_id}` 和 `/jobs/{job_id}/video`。网关把图片或视频写到 `outputs/<request_id>/`，通过 `result.files` 返回对象引用。图片响应体上限为 64 MiB，视频成品上限为 200 MiB。图片调用断线后归还网关槽位并回 MQ 重试。原生图片或尚未保存后端任务 ID 的视频若因网关退出而遗留占用，既有回收循环在领取期限到达后将请求回队；历史已失败的记录只释放占用。

`native_execution_timeout` 默认 180 秒，从每轮领取任务时起算，用于原生模型的首次调用和图片成品发布。上游等待超时后，已经派发的调用继续等原生响应；调用本身失败后回队，原总截止时间不延长。网关记录终态或回队时释放网关槽位，调用方可以用原 `request_id` 查询结果。

## 已有环境切换到模型键

在同一个维护窗口升级网关、使用 Worker 协议的适配器和调用方。每个环境独立切换。先确认实例软删除和 RabbitMQ 投递的迁移已经完成。

1. 暂停新提交。记录各实例的接单状态，再暂停接单，防止维护期间发起新的首次调用。让旧进程完成正在执行的首次调用和结果写入。回读 `requests` 中的 `slot_reserved`、原实例、端点和 `backend_task_id`。有持久后端任务 ID 的视频和可按请求 ID 查询的 Worker 任务可继续回查。原生图片缺少按 ID 查询能力，停止网关前须确认这些执行已结束。旧商品图登记或其他不匹配组合仍有占槽时，先按原账本核实终态。
2. 停止所有旧网关副本和受影响的旧适配器。备份数据库和配置，保留 RabbitMQ 队列、Worker 状态目录和对象账本。用数据库发布账号执行 [模型键迁移](deploy/migrations/model-key-only.sql)，并启用 `ON_ERROR_STOP`。脚本发现历史组合还占槽时会回滚，供旧版继续核对。
3. 将 `units_by_capability` 的每个权重移到 `units_by_model_key` 对应的完整模型键下。旧值与模型键的对应关系见迁移脚本。将适配器配置改为 `VMIND_WORKER_MODEL_KEYS`，并更新实例登记客户端和 Worker 请求格式。启动新适配器及新网关，保持常规提交入口暂停。旧数据库或旧容量配置会阻止新版启动。
4. 回读实例、原请求 ID 和占槽记录。历史商品图登记保留原 ID 并退出实例列表。未派发的旧业务请求进入 `model_retired`，其中已到期的请求使用 `queue_timeout`。现行模型键的排队任务保留原截止时间和投递记录，已派发任务保留原归属和后端任务 ID。验证历史回查后，恢复现行实例的原接单状态。需要重新接单的商品图部署使用 `Vmind_cloth_gen_v1.0` 和新的实例 ID 登记。提交一条新任务验证路由和容量，再恢复常规提交。

历史能力值保存在 `legacy_capability`，供核对迁移前的业务归属。旧 Worker 记录按原指纹去重，旧视频按原任务回查。待旧 Worker 账本全部退出保留范围后，可以删除适配器的旧格式读取分支。

脚本提交前失败时按原版本恢复服务。脚本提交后、新网关启动前，可以用维护窗口的备份恢复原数据库和配置。新网关启动后，后台消费者会补投、处理和回查已有请求。回退前须核对这些写入和执行归属，避免从备份重放已经执行的任务。

## 网关启动

部署前，数据库侧根据 [Ent 模型定义](ent/schema) 准备 `model_gateway` schema、业务表和索引。已有数据库以自动提交方式执行 [RabbitMQ 迁移](deploy/migrations/rabbitmq-dispatch.sql)，并发索引需要独立提交。国内 ACK 使用 `wghub` 数据库。网关运行账号需要该 schema 的访问权限和业务表的读写权限。

上线实例清除功能时，先执行[实例软删除迁移](deploy/migrations/instance-soft-delete.sql)。等网关的所有副本更新后，再发布 Ops 后端和页面。

上线资源组管理前，暂停实例登记，执行 [资源组迁移](deploy/migrations/resource-groups.sql)。脚本按现有组标识回填容量，发现同组容量冲突会回滚。确认所有 Go 网关副本更新后，再恢复实例登记、开放 Ops 管理页。已有请求继续使用原组标识和占槽记录。调整组容量后若需回退旧网关，先核对并同步旧实例列的容量。

国内 ACK 通过 workspace 的 `server_release.py release --service model-gateway --branch dev` 发布两个副本和内网 Service。镜像使用私有不可变仓库 `wg/model-gateway`。首次部署需要 [启动配置清单](deploy/ack-bootstrap.yaml) 和 Kubernetes Secret `model-gateway-runtime`。Nacos `wg.mirror.modelGateway` 保存监听、后端白名单、`rabbitmq_queue` 和容量设置。Secret 提供 `MODEL_GATEWAY_DATABASE_URL`、`MODEL_GATEWAY_RABBITMQ_URL` 和 `MODEL_GATEWAY_{API,ADMIN,BACKEND}_TOKEN`。

国内视频 Worker 设置 `VMIND_NATIVE_VIDEO_PROTOCOL=jobs` 和 `VMIND_NATIVE_VIDEO_TARGET`。目标实例名必须来自原生 `/healthz`，并与网关登记的 GPU 分组对应。提交原生 `/vton` 后持久化后端分配的 job_id，再查询 `/jobs/{job_id}`。原生服务未提供按外部 UUID 查询，受理响应丢失时保留槽位，待核实原执行。图片上游需要 Basic Auth 时，由模型部署提供 `VMIND_NATIVE_USERNAME` 和 `VMIND_NATIVE_PASSWORD`。

适配器镜像从仓库根目录使用 `worker_adapter/Dockerfile` 构建。部署端以稳定的内网 IP 和真实制品、GPU 归属登记实例，并持久挂载 `/state`。成品统一写入目标桶的 `outputs/<request_id>/`。

国内 ACK 通过 `workspace/deploy/server_release.py release --service model-gateway-worker --branch dev` 发布适配器。国内启用 `VMIND_WORKER_STATE_BACKEND=oss`，领取、受理和终态以独占写入保存到同一请求目录，Pod 重建后可继续回查。该 OSS 桶需保持版本控制未启用，确保条件写入拒绝覆盖。记录包含任务身份与状态，成品引用只包含实际媒体对象。

启动网关时必须设置 `XX_WG_REGION`。`CN` 用 `nacos_url` 连接 Nacos。`SG` 和 `US` 用 `appconfig_url` 连接 AppConfig Agent。在 `bootstrap.json` 中填入对应环境的地址。参见 [Nacos 引导示例](config/bootstrap.aliyun.example.json)和 [AppConfig 引导示例](config/bootstrap.aws.example.json)。

启动后，CN 通过 Nacos SDK 监听文档变更，SG 和 US 每 45 秒读取一次本机 AppConfig Agent。新配置校验或绑定新监听地址失败时继续使用上一份配置。

配置正文见 [gateway.example.json](config/gateway.example.json)。同一环境的副本共享 PG schema 和 RabbitMQ 队列，不同环境使用独立队列。网关会声明持久队列，需要配置、写入和读取权限。更换数据库或队列前先排空任务并重启。

Logfire 复用 ModelHub 的 `kangaroo/logs`，将结构化日志和 Trace 发到同一项目。在配置中心增加 `logfire`，沿用同环境 ModelHub 的 `token`、`env` 和 `otel_log_level`，将 `service` 设为 `model-gateway`，`version` 填网关版本。美东路演环境名为 `US_ppe_exhibition`。密钥只通过受控配置传递，示例文件中留空；未配置密钥时仅输出控制台日志。Logfire 配置变化需要重启，热更新会保留原配置并报错。

Logfire 写入权限由同一枚 token 决定，查询权限沿用该项目的成员权限。AWS 任务角色继续读取网关自己的 AppConfig 文档，无需授予读取 ModelHub 文档的权限。日志保留启动阶段、配置更新、MQ 连接和数据库失败信息。readiness 失败包含检查项及 PostgreSQL SQLSTATE，HTTP 仍返回 503。正常探活不产生遥测；业务请求只记录路由模板、状态和耗时，不记录请求头、查询参数、正文、媒体或连接串。`gateway.http_handler` span 只覆盖响应头处理阶段，流式媒体完成和模型终态仍以账本为准。

```sh
go build -o bin/model-gateway ./cmd/model-gateway
export XX_WG_REGION=CN
bin/model-gateway -config /etc/model-gateway/bootstrap.json
```

`/_gateway/live` 检查进程，`/_gateway/ready` 检查请求账本。集成测试使用名称以 `_test` 结尾的独立 PostgreSQL 数据库和测试 RabbitMQ。

```sh
TEST_DATABASE_URL=<test-dsn> TEST_RABBITMQ_URL=<test-amqp-uri> go test -race ./...
python -m unittest discover -s worker_adapter
```

## dev 自动发布

`wgdl666/modelGateway` 的 `dev` 收到 push 后，GitHub Actions 先跑 Go 测试和镜像构建检查。检查通过后，CN dev 和美东路演独立发布同一提交。每个环境按完整提交 SHA 发布镜像，部署后核对运行副本的 digest 和健康状态。

- **CN dev** 使用深圳 VPC 的 ACR 和 ACK `wgdl-dev/default`。创建 `modelgateway-cn-dev-release` runner group，允许 ModelGateway 公开仓使用，将工作流访问限制为 `wgdl666/modelGateway/.github/workflows/test.yml@refs/heads/dev`。组内使用专用 Linux x64 runner。runner 需要 Docker、`jq`、`aliyun` 和 `kubectl`，以及镜像仓库读写、获取短期 kubeconfig、更新 Deployment 和读取健康状态的权限。
- **美东路演** 使用俄亥俄 ECR 和 CloudFormation 栈 `tomirro-model-gateway`，更新 `ppe_exhibition` 的 ECS 服务。由 AWS 管理员在 `us-east-2` 用 [发布角色模板](deploy/aws-exhibition-release-role.yaml) 创建独立的 IAM 角色栈。GitHub Actions 通过 OIDC 获取短期权限，信任范围绑定本仓 `dev` 分支。

作业只更新 Go 网关镜像。需要迁移数据库或变更配置的版本，先在目标环境完成相应准备。网关退出时，正在发出的原生调用可能中断。需要保护这些请求时，在发布窗口暂停相关实例接单并等首次调用结束。

## 模型部署管理

模型部署只使用 `model_gateway.model_deployments` 和 `model_gateway.model_deployment_tasks` 两张表。部署任务、云端数据库记录及 GPU 执行器见 [部署管理指南](deploy/model_deploy/README.md)。该模块按环境显式启用，默认关闭；模型专用加载与配置仍在对应模型仓库，现有生成接口保持不变。
