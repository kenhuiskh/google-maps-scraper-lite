# ── Stage 1: Build the go-rod binary ─────────────────────────────────────────
FROM golang:1.25-bookworm AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    libc6-dev \
    libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*
ENV GOMEMLIMIT=1500MiB GOGC=50
RUN CGO_ENABLED=1 GOOS=linux go build -tags gorod -p 2 -ldflags='-s -w' -o google-maps-scraper-lite .

# ── Stage 2: Minimal runtime ──────────────────────────────────────────────────
FROM debian:bookworm-slim

# Chromium is installed from Debian rather than bundled through Playwright.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    tini \
    chromium \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/google-maps-scraper-lite .

ENV ROD_BROWSER_BIN=/usr/bin/chromium

# tini as PID 1 reaps orphaned Chromium processes left behind when a scraper
# subprocess is OOM-killed or crashes without running its deferred cleanup.
# Default: serve the control UI. Override CMD to run a one-shot scrape.
ENTRYPOINT ["/usr/bin/tini", "--", "/app/google-maps-scraper-lite"]
CMD ["-state-db", "/data/gmdata/scraper-state.sqlite", "-control-addr", "0.0.0.0:8080"]
