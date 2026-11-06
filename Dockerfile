FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -o /model-gateway ./cmd/model-gateway

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 gateway
WORKDIR /app
COPY --from=build /model-gateway /app/model-gateway
COPY migrations/ /app/migrations/
USER gateway
EXPOSE 18580
ENTRYPOINT ["/app/model-gateway"]
