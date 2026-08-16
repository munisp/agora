# syntax=docker/dockerfile:1
# Multi-stage build: rust:1.80-bookworm -> debian:bookworm-slim (SPEC §15).

FROM rust:1.80-bookworm AS build
WORKDIR /app

# cmake is required by rdkafka's `cmake-build` feature (bundled librdkafka).
RUN apt-get update \
    && apt-get install -y --no-install-recommends cmake build-essential pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# Dependency caching layer.
COPY Cargo.toml ./
COPY Cargo.lock ./
RUN mkdir -p src && echo 'fn main() {}' > src/main.rs \
    && cargo build --release \
    && rm -rf src

COPY src ./src
RUN touch src/main.rs && cargo build --release

# To build the live TigerBeetle client instead (ADR-0007):
#   docker build --build-arg CARGO_FEATURES="--features tb-live" ...
ARG CARGO_FEATURES=""
RUN if [ -n "$CARGO_FEATURES" ]; then cargo build --release $CARGO_FEATURES; fi

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 opendesk
USER opendesk
COPY --from=build /app/target/release/payments-service /usr/local/bin/payments-service
EXPOSE 7004
ENV RUST_LOG=info
ENTRYPOINT ["payments-service"]
