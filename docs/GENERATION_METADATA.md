# 精简生成信息

Wardrobe、BodyModel、VTON、视频统一返回 `generation`。原有返回字段继续保留。
模型原生接口：`generation`；modelGateway：`result.generation`。

```json
{
  "model_version": {"checkpoint": "WGDL/example", "revision": "immutable-commit", "sha256": null},
  "effective_prompt": "实际用于最后一次成功生成的完整提示词",
  "pe_status": "applied",
  "parameters": {"seed": 0, "width": 768, "height": 1024, "steps": 8, "guidance": 1.0, "output_format": "png"},
  "timings": {"queue_ms": 0, "pe_ms": 300, "inference_ms": 2400, "total_ms": 2900},
  "retries": {"pe": {"count": 1, "last_reason": "ValueError"}, "generation": {"count": 0}}
}
```

- `request_id` 是单个任务标识；`trace_id` 是调用方选填的业务链路标识。网关保持自己的请求 ID，模型原生接口优先返回 `X-Gateway-Request-ID`；直连时返回本服务 ID。使用现有幂等键重放不会增加模型重试次数。
- `model_version` 是本次执行的权重身份。HF 来源可确认时记录仓库和不可变 revision；本地权重可用文件名和 SHA256 标识，revision 留空。视频还记录实际加载的 `lora` 身份及 `strength`。网关不使用当前 health 或登记镜像版本回填历史权重身份。
- `effective_prompt` 是最后实际用于模型生成的完整文本，包含 PE 及移除衣架、单人限制、透明背景等后处理约束。PE 中间结果仍在原有日志中。
- `pe_status` 为 `disabled`、`applied` 或 `fallback`。回退时增加 `pe_fallback_reason`；某些兼容 VTON 路径会回退到另一种 PE 策略，此时仍可能使用 PE。
- `parameters` 记录实际 seed、输出像素尺寸、步数、guidance、文件格式。VTON 换 seed 重试后返回最终成功 seed。没有适用 guidance 的视频返回 null。视频额外返回 `stage_steps`、`fps`、`num_frames`；两阶段 8+3 的总 `steps` 为 11。
- `retries.<stage>.count` 不含第一次执行。`pe` 是 PE 再调用次数；`generation` 是模型重新生成次数，VTON 单人校验失败的原因是 `person_count_not_one`；`last_reason` 仅在有重试时提供简短原因。缺失表示旧服务未上报，不等同于零。完整逐次记录留在日志中。

所有时间单位为毫秒：

| 字段 | 口径 |
| --- | --- |
| `queue_ms` | 模型服务自己的排队等待；即时执行的 Qwen worker 为 0 |
| `pe_ms` | PE 调用时间，包含 PE 服务内部等待；关闭 PE 时为 0 |
| `inference_ms` | 模型 worker 执行，包括编码、去噪、解码及输出编码；VTON 验证重试路径包含验证和重试 |
| `total_ms` | 模型处理入口至组装结果的耗时；不含调用方网络传输。视频从任务受理计至完成 |
| `gateway_queue_ms` | 网关受理至最后一次派发的等待跨度，重试时包含先前派发及退避时间 |
| `gateway_total_ms` | 网关受理至结果发布完成并记录终态的耗时，包含对象存储上传及视频轮询等待 |

网关额外补充 `retries.gateway_dispatch`。其 count 按数据库中成功领取的派发轮次减一计算，进程重启不清零；取得领取后进程中止也算一次派发轮次，不宣称一定触发了 GPU 推理。网关失败结果通过 `error.retries.gateway_dispatch` 返回已知次数，模型内部失败而没有执行回执时不猜测 PE/生成次数。

阶段存在并行或嵌套，不能直接相加推导总耗时。旧记录缺字段、后端未测量的数值保持 null/缺失。零值 seed、关闭 PE 的 0 毫秒均保留。

模型返回后才升级网关也不会改变已有 `files`。先升级网关时，旧模型没有 `generation` 仍可正常返回文件，网关仅增加自己实际测量的信息。业务应将新增对象视为可选，不依赖历史记录含有它。

## 发布顺序

1. 先在现有 PostgreSQL 执行 `deploy/migrations/generation-retries.sql`；这是添加两列的幂等迁移，兼容旧网关，不改历史结果或释放槽位。默认 schema 是 `model_gateway`，自定义 schema 的部署需替换限定名。
2. 发布新版 modelGateway；就绪检查会检查新增列，缺迁移的新副本不能接流量。
3. 发布 VMind-Image（FLUX、Qwen worker 及兼容 VTON orchestrator）和 VMind-Video worker/gateway。
4. 检查一次真实请求的 `result.generation`，再用相同幂等键查询和重放，确认版本、seed、prompt 和计时不变。

模型 receipt 随现有 `requests.result` JSON 保存，无需媒体重复入库。新增数据库列只用于网关跨副本/重启后的派发重试计数。
