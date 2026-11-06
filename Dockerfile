# ---- Builder ----
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled: modernc.org/sqlite is pure Go.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/logai ./cmd/logai

# ---- Final ----
FROM alpine:3.19

RUN addgroup -S logai && adduser -S logai -G logai

# CA certs for outbound HTTPS to the LLM provider / GitLab / OpenSearch.
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /out/logai /app/logai

# The SQLite DB must live somewhere the non-root user can write. Mount a volume
# at /app/data to keep it across container restarts.
RUN mkdir -p /app/data && chown logai:logai /app/data
ENV DB_PATH=/app/data/logai.db
VOLUME ["/app/data"]

USER logai

EXPOSE 3000

ENTRYPOINT ["/app/logai"]
