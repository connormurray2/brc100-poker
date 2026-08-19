# The toolbox ships no Dockerfile, so this is ours.
#
# Multi-stage: the build stage carries the Go toolchain and module cache, the final image
# carries only the binary. Nothing about the wallet key or the database lives in the image --
# both are supplied at runtime, so an image can be shared without sharing custody.

FROM golang:1.26-alpine AS build

# Build dependencies. CGO is off, so no C toolchain is needed: modernc.org/sqlite is pure Go.
ENV CGO_ENABLED=0 GOOS=linux

WORKDIR /src

# Copy the module files first so dependency download is cached independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Reproducible build: version stamped in, symbol table and DWARF stripped.
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/table ./cmd/table

# ---

FROM alpine:3.21

# Certificates are required: the toolbox talks to arcade and ChainTracks over HTTPS, and a
# missing CA bundle would fail every chain call at runtime rather than at build.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 poker

COPY --from=build /out/table /usr/local/bin/table

# Run unprivileged. The service needs no filesystem write access: its state is in Postgres.
USER 10001:10001

# The table service and its health probes.
EXPOSE 8080

# No default POKER_WALLET_KEY or POKER_POSTGRES_DSN: the service refuses to start without
# them, which is better than starting with a placeholder.
ENV POKER_ENV=production \
    POKER_LISTEN=:8080 \
    POKER_LOG_LEVEL=info

ENTRYPOINT ["/usr/local/bin/table"]
