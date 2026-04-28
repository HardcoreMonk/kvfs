.PHONY: help build test test-race bench clean compose-up compose-down demo-alpha demo-epsilon fmt lint license

help:
	@echo 'Targets:'
	@echo '  build         Build all four binaries to ./bin'
	@echo '  test          Run unit tests'
	@echo '  test-race     Run unit tests with race detector (requires CGO)'
	@echo '  bench         Run all Go benchmarks'
	@echo '  fmt           gofmt -w .'
	@echo '  lint          go vet ./...'
	@echo '  license       Add SPDX + Apache 2.0 short header to every .go (idempotent)'
	@echo '  compose-up    docker compose up -d --build'
	@echo '  compose-down  docker compose down -v'
	@echo '  demo-alpha    Run α (3-way replication) demo'
	@echo '  demo-epsilon  Run ε (UrlKey) demo'
	@echo '  clean         Remove ./bin and local bbolt files'

build:
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-edge  ./cmd/kvfs-edge
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-dn    ./cmd/kvfs-dn
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-cli   ./cmd/kvfs-cli
	go build -trimpath -ldflags='-s -w' -o bin/kvfs-coord ./cmd/kvfs-coord

test:
	go test ./...

test-race:
	go test -race ./...

bench:
	go test -bench=. -run='^$$' -benchmem ./...

license:
	./scripts/add-license-headers.sh

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
