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

# CA certs for outbound HTTPS to Anthropic / GitLab / OpenSearch.
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /out/logai /app/logai

USER logai

EXPOSE 3000

ENTRYPOINT ["/app/logai"]
