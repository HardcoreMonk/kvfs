.PHONY: help build test clean compose-up compose-down demo-alpha demo-epsilon fmt lint

help:
	@echo 'Targets:'
	@echo '  build         Build all three binaries to ./bin'
	@echo '  test          Run unit tests'
	@echo '  fmt           gofmt -w .'
	@echo '  lint          go vet ./...'
	@echo '  compose-up    docker compose up -d --build'
	@echo '  compose-down  docker compose down -v'
	@echo '  demo-alpha    Run α (3-way replication) demo'
	@echo '  demo-epsilon  Run ε (UrlKey) demo'
	@echo '  clean         Remove ./bin and local bbolt files'

build:
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-edge ./cmd/kvfs-edge
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-dn   ./cmd/kvfs-dn
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-cli  ./cmd/kvfs-cli

test:
	go test ./...

test-race:
	go test -race ./...

fmt:
	gofmt -w .

lint:
	go vet ./...

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down -v

demo-alpha:
	./scripts/demo-alpha.sh

demo-epsilon:
	./scripts/demo-epsilon.sh

clean:
	rm -rf bin/ edge-data/ *.db
