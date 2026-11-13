FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY deployment_controller/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt && useradd --uid 10001 --create-home controller
COPY deployment_controller /app/deployment_controller
USER controller
ENV PYTHONUNBUFFERED=1
ENTRYPOINT ["python", "-m", "deployment_controller"]
