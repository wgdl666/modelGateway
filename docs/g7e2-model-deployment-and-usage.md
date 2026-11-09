# G7e2 模型部署与调用

## 机器信息

| 项目 | 值 |
| --- | --- |
| EC2 实例 ID | i-0c575c85289a974e6 |
| 区域 | us-east-2（俄亥俄） |
| 规格 | g7e.24xlarge，4 张 NVIDIA RTX PRO 6000 |
| 公网 IP | 3.139.235.236 |
| 内网 IP | 172.31.3.219 |
| SSH | `ssh aws-g7e2` |

## Decision 模型

Decision 使用 GPU 0、1 的两个 worker。接口监听在 G7e2 本机 [127.0.0.1:8874](http://127.0.0.1:8874)。

| 项目 | 地址 |
| --- | --- |
| 预测接口 | POST [http://127.0.0.1:8874/v1/predict](http://127.0.0.1:8874/v1/predict) |
| 预设任务列表 | GET [http://127.0.0.1:8874/v1/tasks](http://127.0.0.1:8874/v1/tasks) |
| 接口文档 | GET [http://127.0.0.1:8874/docs](http://127.0.0.1:8874/docs) |

### 预设任务

```bash
curl http://127.0.0.1:8874/v1/predict \
  -F 'image=@/path/to/photo.jpg' \
  -F 'task=image_type'
```

### 自定义任务

自定义任务用 `prompt` 替代 `task`，并提交 `image`。每题包含 2～8 个选项。

```bash
curl http://127.0.0.1:8874/v1/predict \
  -F 'image=@/path/to/photo.jpg' \
  -F 'prompt=这件上衣更接近哪一种？
- 卫衣：套头，有帽子或抽绳，面料偏厚。
- 衬衫：有领，门襟用纽扣。'
```

也可以单独传选项。

```bash
curl http://127.0.0.1:8874/v1/predict \
  -F 'image=@/path/to/photo.jpg' \
  -F 'prompt=这件上衣更接近哪一种？' \
  -F 'options=["卫衣","衬衫"]'
```

响应中的 `prediction` 是概率最高的选项，`options` 包含各选项的概率，`timings_ms` 包含耗时。

### 从 Mac 访问

8874 只监听 G7e2 本机。可以通过 SSH 隧道访问。

```bash
ssh -N -L 18874:127.0.0.1:8874 aws-g7e2
```

隧道建立后，本机调用地址为 [http://127.0.0.1:18874/v1/predict](http://127.0.0.1:18874/v1/predict)，文档地址为 [http://127.0.0.1:18874/docs](http://127.0.0.1:18874/docs)。

## VLM 模型

VLM 使用 Qwen3.5-35B-A3B 和 checkpoint-684 LoRA，采用四卡张量并行。调用走美东路演 `modelGateway` 的 OpenAI Chat Completions 接口。

| 项目 | 值 |
| --- | --- |
| 环境 | 美东路演（`ppe_exhibition`） |
| 模型名 | `Vmind_dream_v1`（区分大小写） |
| 对话接口 | `POST http://model-gateway.tomirro.prod:18580/v1/chat/completions` |
| 网关就绪检查 | `GET http://model-gateway.tomirro.prod:18580/_gateway/ready` |

### 鉴权

从获准访问网关的美东路演内网环境发起请求。先将路演网关的业务 API Token 设置为环境变量 `MODEL_GATEWAY_API_TOKEN`。

对话请求携带 `Authorization: Bearer <API Token>` 和 `Content-Type: application/json` 请求头。下面两个示例都从环境变量读取 Token。

### 纯文本调用

```bash
curl --fail-with-body --max-time 125 \
  'http://model-gateway.tomirro.prod:18580/v1/chat/completions' \
  -H "Authorization: Bearer ${MODEL_GATEWAY_API_TOKEN:?请先设置 MODEL_GATEWAY_API_TOKEN}" \
  -H 'Content-Type: application/json' \
  --data '{
    "model": "Vmind_dream_v1",
    "messages": [
      {"role": "user", "content": "你好，请用一句话介绍你自己。"}
    ],
    "max_tokens": 512
  }'
```

### 带图调用

在已安装 `requests` 的 Python 环境中运行。将 `photo.jpg` 放在当前目录，示例会将图片编码为 Base64 Data URL。

```python
import base64
import os
from pathlib import Path

import requests

image_base64 = base64.b64encode(Path("photo.jpg").read_bytes()).decode("ascii")
api_token = os.environ["MODEL_GATEWAY_API_TOKEN"]

response = requests.post(
    "http://model-gateway.tomirro.prod:18580/v1/chat/completions",
    headers={
        "Authorization": f"Bearer {api_token}",
        "Content-Type": "application/json",
    },
    json={
        "model": "Vmind_dream_v1",
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "image_url",
                        "image_url": {
                            "url": f"data:image/jpeg;base64,{image_base64}",
                        },
                    },
                    {
                        "type": "text",
                        "text": "请描述这张图片中的人物和服装。",
                    },
                ],
            }
        ],
        "max_tokens": 512,
        "temperature": 0.2,
    },
    timeout=125,
)

response.raise_for_status()
print(response.json()["choices"][0]["message"]["content"])
```

PNG 图片使用 `data:image/png;base64,` 前缀。回复文本在 `choices[0].message.content` 中，实测内容可包含模型生成的分析文字。
