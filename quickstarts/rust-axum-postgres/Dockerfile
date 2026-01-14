FROM rust:1.74 AS build
WORKDIR /app
COPY Cargo.toml .
COPY src ./src
RUN cargo build --release

FROM debian:bookworm-slim
ENV RUST_LOG=info
WORKDIR /app
COPY --from=build /app/target/release/rust-axum-postgres /app/app
EXPOSE 8080
ENTRYPOINT ["/app/app"]
