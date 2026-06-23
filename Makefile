.PHONY: build run test migrate lint docker-build tidy

BINARY=bin/logai
DB_PATH?=logai.db

build:
	go build -o $(BINARY) ./cmd/logai

run:
	go run ./cmd/logai

test:
	go test ./...

# Apply the SQLite schema against the DB file (pure-Go, no sqlite3 CLI needed).
migrate:
	go run ./cmd/logai -migrate-only

lint:
	golangci-lint run

tidy:
	go mod tidy

docker-build:
	docker build -t logai .
