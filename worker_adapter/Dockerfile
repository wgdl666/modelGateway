ARG PYTHON_RUNTIME_IMAGE=python:3.13-slim
FROM ${PYTHON_RUNTIME_IMAGE}
ARG PIP_INDEX_URL=https://pypi.org/simple
ARG SOURCE_REVISION=""
LABEL org.opencontainers.image.revision="$SOURCE_REVISION"
WORKDIR /app
COPY worker_adapter/requirements.txt .
RUN pip install --index-url "$PIP_INDEX_URL" --timeout 20 --retries 2 --no-cache-dir -r requirements.txt \
    && mkdir /state && chown 10001:10001 /state
COPY worker_adapter/app.py .
ENV VMIND_WORKER_STATE_DIR=/state PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
USER 10001:10001
EXPOSE 8001
# 国内 ACK 由两个容器分别监听图片和视频入口，执行状态保存在 OSS。
ENTRYPOINT ["uvicorn", "app:app", "--host", "0.0.0.0"]
CMD ["--port", "8001"]
