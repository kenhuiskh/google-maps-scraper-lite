# ── Stage 1: Build binary + download Playwright Chromium ─────────────────────
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
RUN CGO_ENABLED=1 GOOS=linux go build -p 2 -ldflags='-s -w' -o google-maps-scraper-lite .

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium

# ── Stage 2: Minimal runtime ──────────────────────────────────────────────────
FROM debian:bookworm-slim

# Chromium runtime dependencies (mirrors what playwright --with-deps installs)
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    tini \
    fonts-liberation \
    libasound2 \
    libatk-bridge2.0-0 \
    libatk1.0-0 \
    libatspi2.0-0 \
    libcairo-gobject2 \
    libcairo2 \
    libcups2 \
    libdbus-1-3 \
    libdrm2 \
    libegl1 \
    libfontconfig1 \
    libfreetype6 \
    libgdk-pixbuf-2.0-0 \
    libgbm1 \
    libglib2.0-0 \
    libgtk-3-0 \
    libnspr4 \
    libnss3 \
    libpangocairo-1.0-0 \
    libpango-1.0-0 \
    libx11-6 \
    libx11-xcb1 \
    libxcb1 \
    libxcomposite1 \
    libxcursor1 \
    libxdamage1 \
    libxext6 \
    libxfixes3 \
    libxi6 \
    libxkbcommon0 \
    libxrandr2 \
    libxrender1 \
    libxshmfence1 \
    libxtst6 \
    xdg-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/google-maps-scraper-lite .
COPY --from=builder /ms-playwright /ms-playwright

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

# tini as PID 1 reaps orphaned Chromium/Node driver processes left behind when a
# scraper subprocess is OOM-killed or crashes without running its deferred cleanup.
# Default: serve the control UI. Override CMD to run a one-shot scrape.
ENTRYPOINT ["/usr/bin/tini", "--", "/app/google-maps-scraper-lite"]
CMD ["-state-db", "/data/gmdata/scraper-state.sqlite", "-control-addr", "0.0.0.0:8080"]
