.PHONY: dev frontend build test mock clean tidy

# Run backend with live-reload friendly flags; frontend dev server proxies /api and /v1.
dev:
	go run ./cmd/llm-switch -web-dev

# Build the React frontend and copy dist into the embeddable location.
frontend:
	cd web && npm ci && npm run build
	rm -rf internal/web/dist
	cp -r web/dist internal/web/dist

# Single-binary release build (embeds frontend).
build: frontend
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/llm-switch ./cmd/llm-switch

test:
	go test ./... -race

# Fake OpenAI+Anthropic upstream for local end-to-end verification.
mock:
	go run ./cmd/mockupstream -addr :9091

tidy:
	go mod tidy

clean:
	rm -rf bin internal/web/dist

run: build
	LLM_SWITCH_ADMIN_PASSWORD=test ./bin/llm-switch -addr 127.0.0.1:8090
