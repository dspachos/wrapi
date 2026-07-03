# wrapi — turn any REST API into an agent-native gateway
#
# Common targets:
#   make deps      install compiler + gateway dependencies
#   make compile   run the design-time compiler (needs LLM_* env; pass URL=...)
#   make build     build the Go gateway -> gateway/bin/gateway
#   make run       build + run the gateway (needs JWT_SECRET + LEGACY_API_URL env)
#   make test      run compiler (node) + gateway (go) test suites
#   make bench     report token reduction (RAW=raw.json AGENT=gateway/config/agent_openapi.json)
#   make docker    build the gateway container image
#   make clean     remove build artifacts

GATEWAY_DIR := gateway
COMPILER_DIR := compiler
BIN := $(GATEWAY_DIR)/bin/gateway

.PHONY: deps compile build run test bench docker clean

deps:
	npm install
	cd $(GATEWAY_DIR) && go mod tidy

# Design-time: fetch the remote OpenAPI spec and emit gateway/config/*.json.
# Requires: LLM_BASE_URL, LLM_API_KEY (LLM_MODEL optional).
# Usage:
#   make compile URL=https://api.example.com/openapi.json
#   make compile URL=https://api.example.com/openapi.json OUT=./out
# If URL is omitted, the CLI falls back to $REMOTE_OPENAPI_URL.
compile:
	node $(COMPILER_DIR)/bin/wrapi.js $(URL) $(OUT)

build:
	cd $(GATEWAY_DIR) && go build -o bin/gateway .

# Run both test suites.
test:
	node --test
	cd $(GATEWAY_DIR) && go test ./...

# Runtime: build then start the proxy.
# Requires: JWT_SECRET, LEGACY_API_URL (GATEWAY_CONFIG_PATH, LISTEN_ADDR optional).
run: build
	cd $(GATEWAY_DIR) && ./bin/gateway

# Report agent-context token reduction.
# Usage: make bench RAW=path/to/origin-openapi.json AGENT=gateway/config/agent_openapi.json
bench:
	node scripts/bench-tokens.mjs $(RAW) $(AGENT)

docker:
	docker build -t wrapi .

clean:
	rm -rf $(GATEWAY_DIR)/bin $(GATEWAY_DIR)/gateway
