# Rust Quickstart: Axum + Postgres with Keploy

## Local Run
- Install Rust toolchain.
- Start Postgres or use Docker Compose.
- Run the app:

```bash
DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres cargo run
```

## Docker Compose
- From this directory:

```bash
docker compose up --build
```

App: http://localhost:8080  
Endpoints:
- GET /users
- POST /users { "name": "Alice" }

## Keploy Record

```bash
keploy record -c "docker compose up --build" --container-name "rust-axum-app"
```

Generate traffic:

```bash
curl -X POST http://localhost:8080/users -H "Content-Type: application/json" -d '{"name":"Alice"}'
curl http://localhost:8080/users
```

Stop the stack when done.

## Keploy Test (Replay)

```bash
keploy test -c "docker compose up" --container-name "rust-axum-app" --delay 10
```

View reports in Keploy output.  
Files:
- [Cargo.toml](./Cargo.toml)
- [main.rs](./src/main.rs)
- [Dockerfile](./Dockerfile)
- [docker-compose.yml](./docker-compose.yml)
