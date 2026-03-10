# CLAUDE.md

## Service Description

**aspirant-server** is a Go/Gin API gateway that serves as the backend for the aspirant-online platform. It provides:

- **Authentication** -- JWT-based login with role-based access control (RBAC)
- **File management** -- Per-user and shared file storage backed by a local filesystem with 50 GB quotas
- **Game logic** -- Word Weaver board processing, game score leaderboards
- **Service proxying** -- Reverse proxy to microservices (transcriber, commander, translator)
- **Asset management** -- S3-backed image and audio asset delivery with semantic name-to-hash mappings
- **Data models** -- PostgreSQL/GORM CRUD for users, roles, messages, feeding times, game scores

## How to Run

### Prerequisites

- **PostgreSQL** -- Required for user/role/message/game data
- **AWS S3** -- Required for asset storage and dictionary files
- **Optional microservices** (only needed if you use their proxy endpoints):
  - `transcriber` -- Voice message transcription (Whisper-based)
  - `commander` -- Voice command parsing and task management
  - `translator` -- Language translation (LibreTranslate-based)

### Local Development

```bash
# Copy and configure environment
cp .env.example .env   # then fill in values

# Run directly
go run main.go

# Or build and run
go build -o main . && ./main
```

### Docker

```bash
docker build \
  --build-arg DB_USER=... \
  --build-arg DB_HOST=... \
  --build-arg DB_PASSWORD=... \
  --build-arg DB_NAME=... \
  --build-arg DB_PORT=5432 \
  --build-arg AWS_ACCESS_KEY_ID=... \
  --build-arg AWS_SECRET_ACCESS_KEY=... \
  --build-arg AWS_REGION=... \
  --build-arg S3_BUCKET_NAME=... \
  -t aspirant-server .

docker run -p 8080:8080 aspirant-server
```

## Port

The server listens on **port 8080**.

## Route Groups

| Group | Auth Required | Roles Allowed | Example Endpoints |
|-------|--------------|---------------|-------------------|
| **Public** | No | Anyone | `POST /login`, `GET /health`, `GET /fetch-object/:etag` |
| **Authenticated** | Yes | Any logged-in user | `GET /data_models/users/:id`, `POST /games/scores` |
| **Trusted** | Yes | Trusted, Admin | `GET /files/list`, `POST /translator/translations` |
| **Admin** | Yes | Admin only | `POST /data_models/users`, `GET /voice-messages`, `POST /commander/process` |

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DB_HOST` | Yes | PostgreSQL host |
| `DB_USER` | Yes | PostgreSQL username |
| `DB_PASSWORD` | Yes | PostgreSQL password |
| `DB_NAME` | Yes | PostgreSQL database name |
| `DB_PORT` | No | PostgreSQL port (default: 5432) |
| `AWS_ACCESS_KEY_ID` | Yes | AWS access key for S3 |
| `AWS_SECRET_ACCESS_KEY` | Yes | AWS secret key for S3 |
| `AWS_REGION` | Yes | AWS region |
| `S3_BUCKET_NAME` | Yes | S3 bucket for assets |
| `GIN_MODE` | No | Gin mode: `debug` or `release` |
| `TRANSCRIBER_URL` | No | Transcriber service URL (default: `http://transcriber:8000`) |
| `COMMANDER_URL` | No | Commander service URL (default: `http://commander:8000`) |
| `TRANSLATOR_URL` | No | Translator service URL (default: `http://translator:8000`) |

## Database Tables

| Table | Description |
|-------|-------------|
| `users` | User accounts with hashed passwords and role references |
| `roles` | RBAC roles: Admin, User, Guest, Gamer, Deleted, Trusted |
| `messages` | Message board entries |
| `game_scores` | Game scores with JSONB metadata and leaderboard support |
| `ludde_feeding_times` | Pet feeding time tracker |

## Conventions

This project follows the conventions defined in the [aspirant-meta](https://github.com/the-anonymous-aspirant/aspirant-meta) repository:

- **DEVELOPMENT_PHILOSOPHY.md** -- AI-assisted development workflow
- **CONVENTIONS.md** -- Code style, commit messages, PR conventions
- **INFRASTRUCTURE.md** -- Docker, CI/CD, and deployment patterns
