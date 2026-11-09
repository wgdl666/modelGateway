ARG PYTHON_RUNTIME_IMAGE=python:3.12-slim
FROM ${PYTHON_RUNTIME_IMAGE}
WORKDIR /app
COPY worker_adapter/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && mkdir /state && chown 10001:10001 /state
COPY worker_adapter/app.py .
ENV VMIND_WORKER_STATE_DIR=/state PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
USER 10001:10001
EXPOSE 8001
# 状态目录由模型部署挂载持久卷，每个目录只运行一个适配器。
ENTRYPOINT ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8001"]
