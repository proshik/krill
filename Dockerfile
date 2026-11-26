# syntax=docker/dockerfile:1

# ---- Builder stage ----
# Pinned to the exact Go version declared in go.mod (go 1.26.11).
# The builder runs on the build machine's own platform ($BUILDPLATFORM) and
# cross-compiles for the target, so a multi-arch buildx build never runs Go
# under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS builder

# The release version, stamped into internal/buildinfo.Version so the running
# server can report it (e.g. via `krill --version`). Passed by CI as
# --build-arg VERSION=<tag>; a local `docker build .` with no arg gets "dev".
ARG VERSION=dev

WORKDIR /src

# Download dependencies first to leverage layer caching.
# The build relies on COMMITTED generated files (internal/web/templates/*_templ.go,
# internal/database/gen/*.go, internal/web/static/app.css) — it does NOT run
# `make generate`, so no templ/sqlc/tailwind tooling is needed in this image.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source (committed generated files are copied in too).
COPY . .

# Build a fully static, stripped binary.
# Migrations (internal/database/migrations/*.sql, via //go:embed in
# internal/database/migrate.go) and web static assets (internal/web/static,
# via //go:embed all:static in internal/web/embed.go) are embedded into the
# binary, so nothing else needs to ship in the final image.
# GOOS/GOARCH come from the target platform; a plain `docker build` sets them
# to the local platform. Declared only here so the steps above stay shared
# between the platforms of one multi-arch build.
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
        -trimpath \
        -ldflags="-s -w -X github.com/proshik/krill/internal/buildinfo.Version=${VERSION}" \
        -o /out/krill \
        ./cmd/krill

# ---- Final stage ----
# distroless/static-debian12 is sufficient: the binary is fully static
# (CGO_ENABLED=0), migrations and assets are embedded, and no shell or extra
# files are needed at runtime. The image is multi-arch (amd64 + arm64) and the
# :nonroot tag runs as UID 65532.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/proshik/krill"

# Only the self-contained binary is required at runtime.
COPY --from=builder /out/krill /usr/local/bin/krill

# Default KRILL_LISTEN_ADDR is :8080 (internal/config/config.go).
EXPOSE 8080

# Already non-root via the :nonroot tag (UID 65532); declared explicitly.
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/krill"]
