VERSION := $(shell git describe --tags --always)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Cross-compile matrix for GitHub Releases. Asset names llm-switch-<goos>-<goarch>
# are matched exactly by `llm-switch upgrade` — do not rename them casually.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: dev frontend build release compress install test mock tidy clean run

# Run backend with live-reload friendly flags; frontend dev server proxies /api and /v1.
dev:
	go run ./cmd/llm-switch --web-dev

# Build the React frontend and copy dist into the embeddable location.
# The .gitkeep keeps `go build ./...` compiling on fresh clones without Node.
frontend:
	cd web && npm ci && npm run build
	rm -rf internal/web/dist
	cp -r web/dist internal/web/dist
	touch internal/web/dist/.gitkeep

# Single-binary release build (embeds frontend).
build: frontend
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/llm-switch ./cmd/llm-switch

# Cross-compile the release assets; the frontend is built exactly once.
release: frontend
	@mkdir -p bin
	@for t in $(PLATFORMS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "building bin/llm-switch-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" \
			-o bin/llm-switch-$$os-$$arch ./cmd/llm-switch || exit 1; \
	done

# Compress release binaries in place; skips silently when upx is missing or
# the format is unsupported (Mach-O arm64).
compress:
	upx --best bin/llm-switch-* || true

install: build
	cp -rf bin/llm-switch ~/.local/bin/llm-switch

test:
	go test ./... -race

# Fake OpenAI+Anthropic upstream for local end-to-end verification.
mock:
	go run ./cmd/mockupstream -addr :9091

tidy:
	go mod tidy

clean:
	rm -rf bin internal/web/dist/*

run: build
	LLM_SWITCH_ADMIN_PASSWORD=test ./bin/llm-switch
