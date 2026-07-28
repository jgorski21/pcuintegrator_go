# syntax=docker/dockerfile:1

# Cloudflare requires linux/amd64 container images.
#
# This ARG must be declared before the first FROM to be visible to the FROM lines
# (global scope). It exists so the platform is not a bare constant, which BuildKit
# lints against — the value is what matters, not the literal.
ARG RUNTIME_PLATFORM=linux/amd64

# The builder runs NATIVELY on the build host (arm64 on an Apple Silicon Mac).
# Nothing amd64 ever executes during the build: Go cross-compiles, so QEMU and
# Rosetta are not involved and the build runs at full speed.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/server .

# The final stage must be a REAL base image pinned to amd64, not `scratch`.
# BuildKit ignores --platform on `FROM scratch` and stamps the output manifest with
# the BUILD host's architecture, so a scratch build on an Apple Silicon Mac yields
# an arm64 image no matter what the binary inside is. Verified empirically:
# `docker image inspect` reported linux/arm64. An amd64 base image makes the
# manifest amd64 regardless of whether wrangler passes --platform to docker build,
# which the Cloudflare docs never state either way.
#
# alpine over distroless/scratch also buys a working shell, so
# `wrangler containers ssh` is usable when something goes wrong at 2am.
FROM --platform=$RUNTIME_PLATFORM alpine:3.22

# MANDATORY. Alpine ships no CA bundle, and without one every HTTPS call to
# api.clickup.com and api.productive.io fails — the classic first-deploy failure.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/server /server

# Required for local `wrangler dev` (in production every port is reachable).
EXPOSE 8080

# Containers run unprivileged on the platform; being explicit costs nothing.
USER 65532:65532

ENTRYPOINT ["/server"]
