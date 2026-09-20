# syntax=docker/dockerfile:1

# Stage 1: build the React admin UI.
FROM node:24-alpine AS web
WORKDIR /build/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: build the single Go binary with the embedded UI.
FROM golang:1.27-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /build/web/dist ./internal/web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/llm-switch ./cmd/llm-switch

# Stage 3: minimal runtime.
FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 llm-switch \
    && mkdir -p /data && chown llm-switch:llm-switch /data
COPY --from=build /out/llm-switch /usr/local/bin/llm-switch
ENV LLM_SWITCH_DATA_DIR=/data \
    LLM_SWITCH_LOG_FORMAT=text
USER llm-switch
VOLUME /data
EXPOSE 8901
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8901/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/llm-switch"]
