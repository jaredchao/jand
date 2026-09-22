.PHONY: build test smoke check

build:
	go build -trimpath -o bin/jand ./cmd/jand

test:
	go test -race ./...

check: test
	go vet ./...

smoke: build
	python3 scripts/smoke.py

.PHONY: release
release:
	python3 scripts/release.py
