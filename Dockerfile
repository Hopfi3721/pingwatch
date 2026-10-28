# Сборка статического бинарника: CGO не нужен, SQLite здесь чистый Go
# (modernc.org/sqlite), поэтому образ-рантайм остаётся минимальным.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: они меняются реже, чем код.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/pingwatch \
    ./cmd/pingwatch

FROM alpine:3.22

# ca-certificates нужны для HTTPS-проверок и Telegram, tzdata — для логов.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 pingwatch \
    && mkdir -p /data \
    && chown pingwatch:pingwatch /data

COPY --from=build /out/pingwatch /usr/local/bin/pingwatch

USER pingwatch
WORKDIR /app

ENV ADDR=:8080 \
    DB_PATH=/data/pingwatch.db

EXPOSE 8080
VOLUME ["/data"]

# busybox wget есть в alpine: отдельный пакет не нужен.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["pingwatch"]
