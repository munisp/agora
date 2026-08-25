# syntax=docker/dockerfile:1
# Multi-stage build: rust:1.98-bookworm -> debian:bookworm-slim (SPEC §15).
# Builder needs cargo >= 1.85: the pinned Cargo.lock pulls icu_normalizer
# 2.3.0 (via reqwest -> url -> idna), which is edition-2024. rust:1.80
# cannot parse it, so the old builder could not build the lock at all.

FROM rust:1.98-bookworm AS build
WORKDIR /app

# cmake is required by rdkafka's `cmake-build` feature (bundled librdkafka).
RUN apt-get update \
    && apt-get install -y --no-install-recommends cmake build-essential pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# F15-01: default build compiles the live Fluvio consumer so a plain
# `docker build` produces a BOOTABLE image — main.rs fails closed (refuses
# to start) when fluvio-live is absent and GATEWAY_EDGE_ALLOW_SIM is unset,
# so shipping a stub-by-default image crash-looped every deploy. The
# fail-closed refusal is kept as defense-in-depth for explicit non-live
# builds:  docker build --build-arg CARGO_FEATURES="" ...
ARG CARGO_FEATURES="fluvio-live"

# Dependency caching layer (pinned lockfile: reproducible builds).
COPY Cargo.toml Cargo.lock ./
RUN mkdir -p src && echo 'fn main() {}' > src/main.rs \
    && if [ -n "$CARGO_FEATURES" ]; then \
         case "$CARGO_FEATURES" in \
           --features*) cargo build --release --locked $CARGO_FEATURES ;; \
           *) cargo build --release --locked --features "$CARGO_FEATURES" ;; \
         esac; \
       fi \
    && rm -rf src

COPY src ./src
RUN touch src/main.rs && if [ -n "$CARGO_FEATURES" ]; then \
      case "$CARGO_FEATURES" in \
        --features*) cargo build --release --locked $CARGO_FEATURES ;; \
        *) cargo build --release --locked --features "$CARGO_FEATURES" ;; \
      esac; \
    fi

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 opendesk
USER opendesk
COPY --from=build /app/target/release/gateway-edge /usr/local/bin/gateway-edge
EXPOSE 7005
ENV RUST_LOG=info
ENTRYPOINT ["gateway-edge"]
