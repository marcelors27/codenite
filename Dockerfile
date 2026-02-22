# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/codenite-worker ./cmd/worker

FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates git

RUN addgroup -S app && adduser -S -u 10001 -G app app
WORKDIR /app

COPY --from=builder /out/codenite-worker /usr/local/bin/codenite-worker
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY config.example.json /app/config.example.json

RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV WORKER_CONFIG_PATH=/app/config.json

USER app
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-config", "/app/config.json"]
