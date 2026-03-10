# aspirant-server

Go/Gin API gateway for the aspirant-online platform. Provides authentication, file management, game logic, and reverse-proxy access to microservices.

## Quick Start

```bash
# 1. Configure environment
cp .env.example .env   # then fill in values

# 2. Start PostgreSQL (e.g. via Docker)
docker run -d --name postgres \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=secret \
  -e POSTGRES_DB=aspirant \
  -p 5432:5432 postgres:15

# 3. Run the server
go run main.go

# 4. Bootstrap first admin
curl -X POST http://localhost:8080/bootstrap/admin \
  -H "Content-Type: application/json" \
  -d '{"user_name": "admin", "password": "changeme"}'
```

The server listens on **port 8080**.

## Route Overview

| Group | Auth | Roles | Examples |
|-------|------|-------|----------|
| Public | None | Anyone | `POST /login`, `GET /health`, `GET /games/scores` |
| Authenticated | JWT | Any user | `GET /data_models/users`, `POST /games/scores` |
| Trusted | JWT | Trusted, Admin | `GET /files/list`, `POST /translator/translations` |
| Admin | JWT | Admin | `POST /data_models/users`, `GET /voice-messages` |

See [docs/SERVER_SPEC.md](docs/SERVER_SPEC.md) for the full route table.

## Architecture

```
  Client
    │
    v
  aspirant-server (:8080)
    ├── PostgreSQL (users, roles, messages, scores, feedings)
    ├── AWS S3 (assets, dictionary)
    ├── Local filesystem (/data/files/ -- 50GB per user)
    └── Microservice proxies
         ├── transcriber (voice-to-text)
         ├── commander (voice command parsing)
         └── translator (language translation)
```

See [docs/SERVER_ARCHITECTURE.md](docs/SERVER_ARCHITECTURE.md) for detailed diagrams.

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DB_HOST` | Yes | -- | PostgreSQL host |
| `DB_USER` | Yes | -- | PostgreSQL user |
| `DB_PASSWORD` | Yes | -- | PostgreSQL password |
| `DB_NAME` | Yes | -- | PostgreSQL database |
| `DB_PORT` | No | 5432 | PostgreSQL port |
| `AWS_ACCESS_KEY_ID` | Yes | -- | AWS access key |
| `AWS_SECRET_ACCESS_KEY` | Yes | -- | AWS secret key |
| `AWS_REGION` | Yes | -- | AWS region |
| `S3_BUCKET_NAME` | Yes | -- | S3 bucket name |
| `GIN_MODE` | No | debug | `debug` or `release` |
| `TRANSCRIBER_URL` | No | `http://transcriber:8000` | Transcriber service |
| `COMMANDER_URL` | No | `http://commander:8000` | Commander service |
| `TRANSLATOR_URL` | No | `http://translator:8000` | Translator service |

## Testing

```bash
go test ./...
```

## Docker

```bash
docker build -t aspirant-server .
docker run -p 8080:8080 aspirant-server
```

See [docs/SERVER_OPERATIONS.md](docs/SERVER_OPERATIONS.md) for detailed build arguments and deployment instructions.

## Documentation

- [SERVER_SPEC.md](docs/SERVER_SPEC.md) -- Full API specification
- [SERVER_ARCHITECTURE.md](docs/SERVER_ARCHITECTURE.md) -- Architecture diagrams
- [SERVER_OPERATIONS.md](docs/SERVER_OPERATIONS.md) -- Setup, run, test, debug
- [CHANGELOG.md](docs/CHANGELOG.md) -- Release history
- [DECISIONS.md](docs/DECISIONS.md) -- Key design decisions
