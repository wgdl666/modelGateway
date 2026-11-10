ARG GO_BUILDER_IMAGE=golang:1.26.4-alpine
ARG GO_RUNTIME_IMAGE=alpine:3.23
# 在构建机原生架构运行 Go，避免 ARM 开发机模拟 amd64 时编译器崩溃；产物仍锁定发布平台。
FROM --platform=$BUILDPLATFORM ${GO_BUILDER_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY="$GOPROXY" go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY ent/ ent/
# 编译临时文件留在内存挂载，避免把大体积 Go 缓存写入容量有限的本地 Docker 磁盘。
RUN --mount=type=tmpfs,target=/tmp CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOCACHE=/tmp/go-cache go build -p 2 -trimpath -o /model-gateway ./cmd/model-gateway

FROM ${GO_RUNTIME_IMAGE}
ARG SOURCE_REVISION=""
LABEL org.opencontainers.image.revision="$SOURCE_REVISION"
# 国内构建机从已锁定的 Go 基础镜像取 CA，避免运行层依赖外部 Alpine 包仓。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN adduser -D -u 10001 gateway
WORKDIR /app
COPY --from=build /model-gateway /app/model-gateway
USER 10001
EXPOSE 18580
ENTRYPOINT ["/app/model-gateway"]
