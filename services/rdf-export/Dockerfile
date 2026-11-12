# syntax=docker/dockerfile:1
# Multi-stage build: rust:1.85-bookworm -> debian:bookworm-slim
# (geo-service/payments-service pattern, SPEC §15). 1.85 (not geo's 1.80)
# because the oxirs 0.4.1 dependency tree (edition-2021 but recent
# transitive deps) needs a newer rustc; the wave toolchain is Rust 1.98.

FROM rust:1.85-bookworm AS build
WORKDIR /app

# Dependency caching layer.
COPY Cargo.toml ./
COPY Cargo.lock ./
RUN mkdir -p src && echo 'fn main() {}' > src/main.rs \
    && cargo build --release \
    && rm -rf src

COPY src ./src
RUN touch src/main.rs && cargo build --release

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 opendesk
USER opendesk
COPY --from=build /app/target/release/rdf-export /usr/local/bin/rdf-export
EXPOSE 7021
ENV RUST_LOG=info
ENTRYPOINT ["rdf-export"]
