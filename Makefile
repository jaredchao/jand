.PHONY: build test smoke check

build:
	go build -trimpath -o bin/jand ./cmd/jand

TEST_FLAGS ?=

test:
	go test -race ./... $(TEST_FLAGS)

check: test
	go vet ./...
	python3 scripts/test_plugin_support.py

smoke: build
	python3 scripts/smoke.py
	python3 scripts/chat_smoke.py
	python3 scripts/plugin_smoke.py
	python3 scripts/watch_smoke.py

.PHONY: release
release:
	python3 scripts/release.py
