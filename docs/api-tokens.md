# 独立 API Token

Ops 的「VMind模型 → API Token」可直接申请和撤销模型调用凭据，无需审批。调用方名称去除首尾空白后必填，最多 100 个字符。名称用于识别调用方，不要求唯一。

Token 默认显示为 `***`，可以反复查看、隐藏或复制。页面刷新、切换环境或离开页面后清除内存中的明文，不写入浏览器持久存储。列表和创建响应只含元数据，查看接口返回完整 Token；响应使用 `Cache-Control: no-store`。

## 存储与权限

modelGateway 在 `model_gateway.api_tokens` 保存 AES-256-GCM 密文。每次加密使用随机 nonce，并绑定 Token ID。服务端可恢复完整凭据，不采用一次性展示方式。列表明确包含已撤销记录供审计；查看和鉴权只接受未软删除记录。

调用方使用 `Authorization: Bearer <token>`。独立 Token 具有普通模型 API 权限，也可读取当前环境全部历史请求的列表、详情及输入输出媒体（`GET /v1/admin/requests`、`GET /v1/admin/requests/:id`、`GET /v1/admin/requests/:id/media`），不按调用方归属隔离。其他管理接口仍只接受管理凭据，独立 Token 不能启用实验参数。现有共享 `api_token`、实验凭据和管理凭据保持各自用途。独立 Token 不增加模型级授权、配额或任务租户隔离。

每次请求查询持久状态，不缓存鉴权结果。撤销生效后，新到达的鉴权请求返回 401；已经通过鉴权或受理的请求继续执行。重复撤销不改变第一次撤销时间。

队列和同步任务的请求账本保存 `api_token_id`，固定凭据及历史记录留空。不同 Token 不能复用同一个幂等请求 ID。

Ops 使用当前环境的管理凭据代理接口。创建人来自已验证的 Ops 会话，浏览器不能指定创建人。有该管理入口访问权限的用户可以查看、复制和撤销当前环境的 Token。

## 发布前置条件

1. 在目标环境显式执行 `deploy/migrations/api-tokens.sql`，再发布网关。脚本增加 Token 表、请求归属列和更新时间触发器，不创建外键。
2. 为该环境所有网关副本注入相同的 `MODEL_GATEWAY_TOKEN_ENCRYPTION_KEY`。值为随机 32 字节密钥的标准 Base64 编码，由 Secret 管理，不写入业务 JSON、仓库或日志。不同环境使用不同密钥。
3. 发布网关后发布 Ops，验证创建、查看、模型鉴权和撤销。本文不代表已执行发布。

未设置加密密钥时，申请、列表和查看接口返回 503，独立 Token 不能鉴权；固定凭据仍可使用。撤销接口不依赖解密密钥。密钥不支持热更新，不能直接替换已有密钥，否则已有 Token 无法解密。后续轮换需要单独迁移密文。

## 管理接口

所有接口要求网关管理凭据，由 Ops 代理访问。

| 方法与路径 | 输入或输出 |
| --- | --- |
| `GET /v1/admin/api-tokens` | `{items: [...]}`，包含名称、创建人、创建时间和撤销时间，不包含 Token |
| `POST /v1/admin/api-tokens` | 输入 `{name, created_by}`，返回创建记录，HTTP 201 |
| `GET /v1/admin/api-tokens/:id` | 返回 `{token}`；不存在或已撤销返回 404 |
| `DELETE /v1/admin/api-tokens/:id` | 软删除，返回 `{revoked: true}`，可重复调用 |

Ops 对应路径为 `/api/vmind/api-tokens` 和 `/api/vmind/api-tokens/{id}`，按 `X-Ops-Region` 路由。Ops 创建接口仅接收 `{name}`。
