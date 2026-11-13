# 区域模型部署生命周期 v1

每个区域有独立的 PostgreSQL、请求队列和网关逻辑地址。网关副本共享区域内登记，部署端只向逻辑地址登记一次。发布控制器使用同一数据库中的操作账本和物理资源组租约；深圳、美东分别执行和恢复，不做跨区域事务或自动流量转移。

## 身份和兼容性

- `model_key`：现有接口契约，升级权重不改调用名称。
- `artifact_digest`：规范化不可变 release JSON 的 SHA-256（`sha256-...`）。内容包含实际 LoRA SHA、底模/推理参数、模型绑定、源码和模型文件哈希。
- `deployment_id`：本区域本 worker 的一次部署。换 release 创建新 ID；普通进程重启保留 ID。
- `boot_id`：模型进程每次加载生成的新 ID。只作实际执行证据，不进入幂等请求指纹。
- `instance_id`：一个模型契约的一条登记。同 worker 的业务接口、通用接口使用各自 ID，共享同一物理 GPU 资源组。

`deployment_id` 为空的历史登记保持兼容。设置该字段后，网关探活必须收到本区域、相同 deployment、artifact 和非空 boot；派发发送 `X-Expected-Deployment-ID`、`X-Expected-Artifact-Digest`、`X-Expected-Model-Key`。worker 在执行锁内核验，响应回传身份，网关在发布结果前再次比较。仅 `ready:true` 不能放行受管理部署。不要给未支持该契约的 worker 填写 deployment_id。

VMind-Image 的 Qwen worker 是首个实现：既有 BodyModel/VTON、通用接口均支持；其他执行器必须实现同一契约并通过验收后才能启用。网关本身不加载模型。

## 区域初始化

先应用 `deploy/migrations/deployment-lifecycle.sql`，再发布网关。迁移只增加列和生命周期表，不修改历史身份、请求或容量。为该环境**显式**写入唯一归属：

```sql
INSERT INTO model_gateway.deployment_domain(id,region) VALUES(1,'cn-shenzhen/dev');
-- 美东数据库使用另一个值，例如 us-east/exhibition；不能对同一库改名冒充另一个区域。
```

网关管理接口 `GET /v1/deployments/domain` 返回区域和协议版本。控制器同时核对该接口与数据库单例，错误配置、缺表、区域不符均停止。管理接口仍需管理凭据。

控制器数据库账号仅需访问 `deployment_domain`、`deployment_operations`、`deployment_leases`；已有网关运行账号额外需要读 `deployment_domain`、`deployment_leases`。迁移与权限由数据库发布账号执行。凭据通过区域 Secret / mode-600 环境文件注入，不写入清单、命令行、worker 或源码。

## 发布入口

安装 `deployment_controller/requirements.txt`，在仓库根目录执行。`regions.example.json` 中的地址和 SSH argv 需要用环境绑定替换；确认 SSH 身份、known_hosts 和 Python 安装，不复制跨主机凭据。

```sh
python -m deployment_controller --config /etc/model-deployment/regions.json \
  apply /releases/body-v2/plan.json --operation body-v2-gpu1
python -m deployment_controller --config /etc/model-deployment/regions.json \
  status --region cn-shenzhen/dev --operation body-v2-gpu1
python -m deployment_controller --config /etc/model-deployment/regions.json \
  watch --region cn-shenzhen/dev
```

`watch` 只接管已经提交、无有效持有者的未完成操作，失败重试有退避，不创建新的发布意图。允许多个控制器运行；同一 worker 同时只能有一个有效持有者。`apply` 网络失败后使用原 operation 和原 plan 重试。变更原 plan 会被拒绝。

单 worker 清单：

```json
{
  "schema_version": 1,
  "region": "cn-shenzhen/dev",
  "worker_id": "5090-2-gpu1",
  "action": "upgrade",
  "resource_groups": ["cn-dev/qwen-image21/gpu1"],
  "instances": [{
    "instance_id": "body-v2-gpu1-business",
    "model_key": "Vmind_bodymodel_v1.0",
    "protocol": "body_model",
    "deployment_id": "body-v2-gpu1",
    "artifact_digest": "sha256-<release JSON SHA-256>",
    "endpoint": "https://HOST/vworld/gpu1",
    "health_url": "https://HOST/vworld/gpu1/v1/images/models/Vmind_bodymodel_v1.0/health",
    "resource_group": "cn-dev/qwen-image21/gpu1",
    "machine_id": "seetacloud/HOST",
    "gpu_devices": [1],
    "capacity_units": 1
  }]
}
```

这是结构示例，digest/host 必须换为实际值；共享 worker 的其他已有绑定（例如 VTON、通用接口）必须全部列出。遗漏会在排空前失败。物理组必须已存在且属于实际 GPU；发布不自动改容量或迁移硬件归属。

`action` 支持 online、upgrade、rollback、offline。回滚以旧 release 创建新的部署/实例 ID，不复活已软删除的登记。offline 的 instances 必须为空。`serve` 可以显式指定新登记是否接单；未指定时继承同模型/端点原来的暂停状态，新能力默认为开放。

多区域/多 worker 使用 `{"targets":[plan1,plan2]}`。每个目标有稳定派生操作 ID；`--region` 只选目标，不改变其操作 ID。某区域失败时其他目标可以继续；输出明确列出部分完成状态。

## 操作阶段与失败语义

`planned → prepared → draining → drained → applied → verified → registered → serving → complete`。

1. 校验已安装制品和原配置，冻结受影响的全部登记。
2. 按物理组排空全部能力，等待网关 reserved requests/units 为零。
3. 部署 agent 另外确认实际 worker 空闲、没有不确定的执行或 pending 幂等记录。
4. 使用 host-installed hook 激活或停止；未知执行结果禁止自动重做。
5. 验证实际加载的每个绑定和 boot，真实功能 smoke 必须通过。
6. 新登记 initially_draining=true；网关验证实际身份后才恢复允许接单的绑定。
7. 旧登记确认无占槽、已暂停且不可接单后软删除，保留请求历史。

未完成操作始终持有逻辑所有权。租约过期只允许**原操作**被新控制器接管；不会自动恢复流量或允许另一份发布覆盖。PostgreSQL 中的递增 epoch 同时保护网关管理写入和宿主机 agent；旧控制器迟到的 resume/register/delete 会被拒绝。agent 用持久文件锁序列化本机动作，并记录动作开始和完成回执；控制器崩溃后可复用完成回执，agent 自身崩溃造成的未知动作需要人工核实，不能猜测成功或重新启动。

发布途中不要用旧脚本直接改服务或绕过网关发送业务流量。未统一入口的手工操作无法受发布租约保护；worker 身份校验仍会阻止旧登记执行新版。客户端断线不证明 GPU 停止，不能用超时清除占用。受管理的原生业务接口若无法确认执行结果，会记录 deployment_execution_unknown 并保留占槽，等待人工核对；通用接口沿用持久幂等和固定实例重试。上线后暂时探活失败只是停止新派发，不自动注销部署。

同一物理 worker 只属于一个区域控制域。agent 的固定 region/worker 配置和区域数据库都校验归属，不能把同一 GPU 同时放入两套独立容量账本。跨区域请求转移不在 v1 范围；请求仍归属最初受理区域。

## Agent 与引擎适配

agent 是 `python -m deployment_controller.agent --config ... --state ...`，通过 SSH stdin 接收受限动作。它只执行宿主机管理员预先安装、由 plan SHA 选择的 argv，不执行请求里的 shell 命令。配置不可被 group/others 写入；state、回执、日志保留在持久目录。

每份已批准 plan 配置 prepare、idle、activate、verify、stop 五个 hook。idle 必须返回 `ok:true,idle:true,active_requests:0,uncertain_executions:0`；verify 在线时返回 `ok:true,smoke_passed:true,boot_id,bindings`，离线时返回 `ok:true,stopped:true`。bindings 的 model_key/deployment_id/artifact_digest 必须逐项等于发布清单。

VMind-Image 提供 `deploy/model_gateway/qwen_host.py` 适配现有 split supervisor。Docker/systemd 及其他引擎可以实现同样 hook，不必迁移到 Kubernetes。更换 hook 实现后需要重新安装获批 plan。

## 验证

Go 集成测试需隔离 `_test` PostgreSQL 和测试 RabbitMQ。Python ledger 测试使用同一测试库，但与 Go 集成测试顺序执行：

```sh
go generate ./ent
TEST_LIFECYCLE_PYTHON=/path/to/python go test -race ./...
python -m unittest discover -s deployment_controller/tests
```

覆盖多副本竞争、旧 epoch 拒绝、区域错配、阶段恢复、重复上线、共享能力遗漏、已有暂停状态、忙碌时不切换及未知 hook 不重复执行。模型端还需验证实际部署出图、跨 profile 切换、重启回放和版本变化拒绝旧幂等键。CPU 模拟测试不能代替最终 GPU 验收。
