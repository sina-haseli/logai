FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mockgitlab .

FROM alpine:3.19
RUN adduser -S app
COPY --from=builder /out/mockgitlab /app/mockgitlab
USER app
EXPOSE 8080
ENTRYPOINT ["/app/mockgitlab"]
