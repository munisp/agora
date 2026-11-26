# syntax=docker/dockerfile:1
# Multi-stage build: rust:1.90-bookworm -> debian:bookworm-slim
# (payments-service Dockerfile pattern, SPEC §15).

# 1.90: Cargo.lock now pins crates needing rust >= 1.90 (terrio, uuid 1.29); 1.80 only built from layer cache.
FROM rust:1.90-bookworm AS build
WORKDIR /app

# libsqlite3-sys (terrio `tiles` -> `sqlite` feature) compiles bundled SQLite.
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Dependency caching layer.
COPY Cargo.toml ./
COPY Cargo.lock ./
RUN mkdir -p src && echo 'fn main() {}' > src/main.rs \
    && cargo build --release \
    && rm -rf src

COPY src ./src
COPY migrations ./migrations
# gazetteer.rs embeds the NG seed CSV (include_str!), so the build needs it too.
COPY seeds ./seeds
RUN touch src/main.rs && cargo build --release

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 opendesk
USER opendesk
COPY --from=build /app/target/release/geo-service /usr/local/bin/geo-service
EXPOSE 7015
ENV RUST_LOG=info
ENTRYPOINT ["geo-service"]
