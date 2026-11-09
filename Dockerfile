ARG GO_BUILDER_IMAGE=golang:1.26.4-alpine
ARG GO_RUNTIME_IMAGE=alpine:3.23
FROM ${GO_BUILDER_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY="$GOPROXY" go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY ent/ ent/
RUN CGO_ENABLED=0 go build -trimpath -o /model-gateway ./cmd/model-gateway

FROM ${GO_RUNTIME_IMAGE}
ARG SOURCE_REVISION=""
LABEL org.opencontainers.image.revision="$SOURCE_REVISION"
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 gateway
WORKDIR /app
COPY --from=build /model-gateway /app/model-gateway
USER 10001
EXPOSE 18580
ENTRYPOINT ["/app/model-gateway"]
