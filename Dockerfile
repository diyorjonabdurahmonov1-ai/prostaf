# ── Stage 1: Build ───────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /src

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build (no CGO needed — pure Go PG driver)
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/prostaff ./cmd/server

# ── Stage 2: Runtime ─────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -S prostaff && adduser -S prostaff -G prostaff

WORKDIR /app

COPY --from=builder /bin/prostaff .
COPY --from=builder /src/swagger.yaml .

# Where uploaded photos are written. Created here, and owned by the user the
# server runs as, because a Docker volume takes its ownership from the directory
# it is mounted over the first time: without this the volume lands root-owned
# and the first upload fails with "permission denied" on a box that looked fine.
RUN mkdir -p /data/uploads && chown -R prostaff:prostaff /data
VOLUME ["/data/uploads"]

USER prostaff

ENV PORT=8000
ENV DEV_MODE=false
ENV UPLOAD_DIR=/data/uploads

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:8000/api/v1/healthz || exit 1

ENTRYPOINT ["./prostaff"]
