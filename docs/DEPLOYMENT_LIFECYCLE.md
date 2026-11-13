# 模型服务自注册生命周期

模型进程完成加载和自检后，主动向所属区域的逻辑网关地址登记；运行时续约；退出时排空并注销。模型如何启动、更新 checkpoint 或重启由现有部署系统负责。无需额外控制器、SSH 发布程序或宿主机 agent。

深圳和美东使用独立 PostgreSQL、队列、网关入口和 worker 凭据。区域内多个网关副本共享同一账本，注册、心跳和退出可以落在不同副本上。一个物理 worker 只归属一个区域，不能跨区域重复计算容量。

## 身份和调度

- `model_key` 保持调用契约，换 checkpoint 不必改名。
- `artifact_digest` 绑定实际权重、底模、推理参数和源码；稳定逻辑文件名参与摘要，本地路径不参与。
- `deployment_id` 标识加载的部署；`boot_id` 每次进程启动重新生成。
- 网关从 worker、deployment、boot、model 派生实例 ID。同一进程重试登记幂等，新进程不会复活旧实例。
- 同一 worker 的所有业务/通用模型绑定在一个事务中登记，共享预先配置的物理资源组。v1 支持单 GPU worker；其他同步/多卡引擎接入前需要对应实现。

网关验证实际健康响应的 region、worker、deployment、boot 和 artifact 后才登记。派发携带 `X-Expected-Deployment-ID`、`X-Expected-Boot-ID`、`X-Expected-Artifact-Digest`、`X-Expected-Model-Key`；模型在执行锁内核对，网关发布结果前再次核对。回查也沿用原进程身份。

## 区域初始化与权限

依次执行 `deploy/migrations/deployment-lifecycle.sql` 和 `deploy/migrations/worker-registration.sql`，然后发布新版网关。显式建立区域单例：

```sql
INSERT INTO model_gateway.deployment_domain(id,region) VALUES(1,'cn-shenzhen/dev');
```

美东使用自己的区域名称和数据库，不能覆盖已有区域归属。深圳已于 2026-11-13 执行第一份迁移；自注册新增列仍需第二份迁移。早期方案的 deployment_operations/deployment_leases 表不再使用，保留即可，不自动删除。

运维使用现有管理 API 预建物理资源组。通过网关 Secret 环境变量 `MODEL_GATEWAY_WORKER_REGISTRATIONS` 注入每个 worker 的授权。该字段不从 Nacos/AppConfig 正文读取。例：

```json
{
  "5090-2-gpu1": {
    "token": "<独立随机 worker 凭据，至少 32 字符>",
    "region": "cn-shenzhen/dev",
    "bindings": [{
      "model_key": "Vmind_bodymodel_v1.0",
      "protocol": "body_model",
      "endpoint": "https://HOST/vworld/gpu1",
      "health_url": "https://HOST/vworld/gpu1/v1/images/models/Vmind_bodymodel_v1.0/health",
      "resource_group": "cn-dev/qwen-image21/gpu1",
      "machine_id": "seetacloud/HOST",
      "gpu_devices": [1],
      "capacity_units": 1
    }]
  }
}
```

共享进程的 VTON、通用接口等必须完整列入 bindings；它们仍只共享一份 GPU 容量。授权固定模型键、协议、地址、物理归属和容量，worker 请求只能报告加载的版本。worker 凭据不能调用管理接口、读取业务请求或操作其他 worker；不要向模型机器发放管理 Token 或数据库凭据。

## 服务协议

使用 worker 专用 Bearer 凭据调用 `POST /v1/workers/{worker_id}/{action}`。

register 请求：

```json
{
  "region":"cn-shenzhen/dev",
  "deployment_id":"body-step1446-gpu1",
  "boot_id":"<进程启动时生成>",
  "bindings":[{"model_key":"Vmind_bodymodel_v1.0","artifact_digest":"sha256-<实际 release 摘要>"}]
}
```

`heartbeat`、`drain`、`unregister` 使用相同 region/deployment_id/boot_id，不需要 bindings。

- **register**：验证完整绑定和实际健康，原子登记；已有正常进程或未解决的物理组占槽阻止替换。
- **heartbeat**：数据库时间续约 60 秒；建议每 15 秒一次。续约不能清除管理员暂停，也不能取消进程已开始的排空。
- **drain**：整个 worker 停止新派发，返回 `reserved_requests`。原请求继续在原地址查询、取结果。
- **unregister**：必须已排空且该进程没有占槽请求，软删除所有绑定。重复注销成功，历史记录保留。

租约过期后，调度事务立即停止派发，不依赖后台清理任务。过期 boot 不能通过迟到心跳或再次注册续命。模型客户端也按本地单调时钟提前关闭接单，网络失败不延长有效期；恢复需启动新进程身份。新进程只能在旧进程已排空或租约过期、且物理组无占槽后登记。

超时、连接断开、进程退出都不等于 GPU 已停止。结果不确定的请求继续占槽，阻止替换；必须核对原执行终态，不能靠注销清除。正常退出时模型应先关闭生成入口、发 drain，保持结果查询和下载服务，等网关占槽及本机执行归零，再注销、退出。

## 兼容迁移与回滚

历史实例没有 worker_id 时保持原行为。首次接入需暂停该物理 worker 的全部旧登记并等待原任务结束；自注册会拒绝遗漏能力或仍在接单的旧登记。管理员暂停会被继承，首次接入验收后通过管理 API 恢复需要接单的绑定。后续正常重启由模型自动登记，继续保留管理员暂停设置。

更换 checkpoint 仍使用现有启动方式：旧进程正常退出，新进程加载自检后注册。回滚以旧制品创建新的 deployment/boot，不复活历史实例。自动注册不会下载权重、修改启动配置或重启模型。

开启自注册之后不要直接回退到不检查租约的网关。先排空并停止受管理 worker，确认无占槽，恢复兼容登记和服务后再回退。数据库只做增量迁移，不回滚请求历史。

## 验证

隔离 `_test` PostgreSQL 与测试 RabbitMQ 下运行 `go test -race ./... -count=1` 和 `go vet ./...`。测试覆盖跨副本注册/续约、并发 boot、完整能力绑定、过期停派、迟到心跳、管理员暂停、正常注销以及未知执行保留。模型端测试注册/续约、断网后本地停派、启动与退出竞态、信号排空和实际 boot 校验；CPU 测试不能代替正式 GPU 出图验收。
