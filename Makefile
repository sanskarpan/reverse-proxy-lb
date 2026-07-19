.PHONY: build test test-race cover bench run run-backends stop-backends lint fmt vet tidy docker compose clean all

build:
	go build -o bin/proxy ./cmd/proxy/
	go build -o bin/test_server ./test/

# Unit + integration tests (self-contained; no external backends needed).
test:
	go test ./...

test-race:
	go test -race -count=1 ./...

cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

bench:
	go test -bench=. -benchmem -run='^$$' ./...

run: build
	./bin/proxy --config configs/config.yaml

run-backends: build
	./bin/test_server 8001 server1 &
	./bin/test_server 8002 server2 &
	./bin/test_server 8003 server3 &
	@echo "Backend servers started on ports 8001, 8002, 8003"

stop-backends:
	pkill -f "test_server" || true

fmt:
	gofmt -w internal/ cmd/ test/

vet:
	go vet ./...

# Requires staticcheck (go install honnef.co/go/tools/cmd/staticcheck@latest).
lint: vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed; ran go vet only"

tidy:
	go mod tidy

docker:
	docker build -t rplb:latest .

compose:
	docker compose up --build

clean:
	rm -rf bin/ coverage.out

all: tidy fmt vet test-race build
