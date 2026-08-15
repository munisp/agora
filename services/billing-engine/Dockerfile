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
# The schema is embedded via include_str!("../migrations/0001_init.sql").
COPY migrations ./migrations
RUN touch src/main.rs && cargo build --release

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 opendesk
USER opendesk
COPY --from=build /app/target/release/billing-engine /usr/local/bin/billing-engine
EXPOSE 7012
ENV RUST_LOG=info
ENTRYPOINT ["billing-engine"]
