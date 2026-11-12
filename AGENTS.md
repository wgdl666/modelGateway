# modelGateway 仓库规则

## 共享规则入口

- 开始任务先读取 WG 工作区公共 `AGENTS.md`，再应用本仓库特例。
- 普通检出读取主仓库上一级的 `AGENTS.md`。Git worktree 用 `git rev-parse --path-format=absolute --git-common-dir` 定位主检出的 `.git`，再读取主检出上一级。不要根据 worktree 的父目录猜位置。
- 公共文件不可达时说明缺口，不把它当作已加载；不为补路径向仓库写入个人绝对路径。

## 请求协议与账本

- 网关负责模型请求受理、排队、实例选择、占槽和执行恢复；具体接口见 `README.md`。请求事实存于 `model_gateway` schema 的 `requests`、`model_instances` 和 `resource_groups`。
- UUID `Idempotency-Key` 同时作为请求主键和模型端查询 ID。同键同业务输入复用原任务，模型或业务输入变化返回 409；追踪字段不改变任务身份。
- 每个 `model_key` 固定一种业务，输入校验、实例匹配和后端路由使用同一键。商品图和人物拆衣使用各自模型键，业务含义变化使用新键。
- 图片持久入队后同步等待，视频持久入队后返回 202。结果以 `result.files[]` 的 provider、region、bucket、key 返回。只允许取消尚未派发的请求，已派发返回 409。

## 调度、容量与恢复

- PostgreSQL 账本保存状态、截止时间、领取标识、实际实例、制品、容量及后端任务归属。RabbitMQ 消息只带请求 ID；受理事务保存待投递时间，Broker 确认持久消息后才清除。
- 派发同时检查实例和共享资源组容量。同一物理卡上的不同模型共用资源组，不能按模型各算一份容量。没有健康且可接单的容量时保持排队，不能当作已开始模型执行。
- 状态为 `queued`、`processing`、`succeeded`、`failed`。回队重试沿用原请求 ID、输入和总截止时间，每轮派发使用独立领取标识，旧结果不能覆盖新一轮。
- 已持久化后端任务 ID 的异步任务继续查询原任务。回队可能造成后端重复执行；断线、相同请求 ID 或网关释放槽位都不能证明 GPU 已停止或 Worker 已去重。
- Worker 适配器的状态目录随同一实例身份持久保存，每个目录只运行一个进程。适配器中断且无法确定已发送图片请求的终态时，保留占槽并核实原执行。
- 实例清除须在同一事务确认已暂停、未就绪且无占槽请求，执行软删除；历史请求保留原实例归属。调整容量或部署身份时，不改写已有请求的归属。

## 鉴权与后端访问

- API、管理、实验凭据权限分开。实验凭据默认关闭；启用后只额外放行协议列出的生成参数，不能访问管理接口。普通 API 凭据仍使用服务端生成默认值。
- 后端地址受 `backend_cidrs`、`backend_public_hosts` 和 `backend_ports` 约束。公网使用 HTTPS，每次建连校验全部 DNS 结果并连接已校验 IP，TLS 验证原域名；关闭代理和重定向。
- G6 视觉入口只转发已列明的路由和字段，图片直接置于请求体；不接收 URL 或对象存储路径，也不进入生成任务队列。

## 配置、生成与验证入口

- 必须指定 `XX_WG_REGION`。CN 用 Nacos，SG/US 用本机 AppConfig Agent；bootstrap 和 JSON 配置示例在 `config/`。同一环境共享 PG schema 与 RabbitMQ 队列，不同环境使用独立队列。
- 数据库或队列变更先排空并重启；Logfire 配置变化也需重启。协议、旧账本迁移和回滚按 `README.md`、`deploy/CN-ENDPOINTS.md` 及 `deploy/migrations/` 的对应步骤执行。
- Ent 生成命令：`go generate ./ent`。正式发布服务名为 `model-gateway`；Worker 适配器为 `model-gateway-worker`，发布入口遵循公共规则。
- `/_gateway/live` 检查进程，`/_gateway/ready` 检查请求账本；二者不代表模型结果已完成。
- Go 集成测试使用以 `_test` 结尾的独立数据库和测试 RabbitMQ：`TEST_DATABASE_URL=<test-dsn> TEST_RABBITMQ_URL=<test-amqp-uri> go test -race ./...`。适配器测试：`python -m unittest discover -s worker_adapter`。按实际改动选择验证范围。
