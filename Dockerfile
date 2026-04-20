# ── Stage 1: Build binary + download Playwright Chromium ─────────────────────
FROM golang:1.25-bookworm AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o google-maps-scraper-lite .

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium

# ── Stage 2: Minimal runtime ──────────────────────────────────────────────────
FROM debian:bookworm-slim

# Chromium runtime dependencies (mirrors what playwright --with-deps installs)
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    fonts-liberation \
    libasound2 \
    libatk-bridge2.0-0 \
    libatk1.0-0 \
    libcairo2 \
    libcups2 \
    libdbus-1-3 \
    libdrm2 \
    libgbm1 \
    libglib2.0-0 \
    libnspr4 \
    libnss3 \
    libpango-1.0-0 \
    libx11-6 \
    libxcb1 \
    libxcomposite1 \
    libxdamage1 \
    libxext6 \
    libxfixes3 \
    libxkbcommon0 \
    libxrandr2 \
    xdg-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/google-maps-scraper-lite .
COPY --from=builder /ms-playwright /ms-playwright

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# Default: run the binary directly.
# Coolify cron jobs override this with: bash /data/gta-sweep.sh
ENTRYPOINT ["/app/google-maps-scraper-lite"]
