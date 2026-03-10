# Server Architecture

## System Overview

```
                              aspirant-server (:8080)
  ┌──────────────────────────────────────────────────────────────────┐
  │                                                                  │
  │   ┌─────────────┐    ┌──────────────┐    ┌──────────────────┐   │
  │   │  Gin Engine  │───>│  Middleware   │───>│   Route Groups   │   │
  │   │  (HTTP)      │    │  CORS, Logs  │    │  Public/Auth/    │   │
  │   │              │    │  JWT Auth     │    │  Trusted/Admin   │   │
  │   └─────────────┘    └──────────────┘    └───────┬──────────┘   │
  │                                                   │              │
  │          ┌────────────────────────────────────────┼───────┐      │
  │          │                    │                    │       │      │
  │          v                    v                    v       v      │
  │   ┌─────────────┐    ┌──────────────┐    ┌──────┐ ┌─────────┐  │
  │   │  Handlers    │    │  Data Models  │    │ S3   │ │ Proxy   │  │
  │   │  (business   │    │  (GORM ORM)   │    │ Func │ │ Clients │  │
  │   │   logic)     │    │              │    │      │ │         │  │
  │   └──────┬──────┘    └──────┬───────┘    └──┬───┘ └────┬────┘  │
  │          │                   │               │          │        │
  └──────────┼───────────────────┼───────────────┼──────────┼────────┘
             │                   │               │          │
             v                   v               v          v
      ┌─────────────┐   ┌──────────────┐  ┌─────────┐  ┌────────────┐
      │  Local Files │   │  PostgreSQL   │  │  AWS S3  │  │ Microsvcs  │
      │  /data/files │   │  (users,      │  │  (assets,│  │            │
      │              │   │   roles,      │  │   dict)  │  │ transcriber│
      │  50GB/user   │   │   messages,   │  │          │  │ commander  │
      │  50GB shared │   │   scores,     │  │          │  │ translator │
      │              │   │   feedings)   │  │          │  │            │
      └─────────────┘   └──────────────┘  └─────────┘  └────────────┘
```

## Package Structure

```
aspirant-server/
├── main.go                          # Entry point: env loading, DB/S3 init, Gin setup
├── go.mod / go.sum                  # Dependencies (module: aspirant-online)
├── Dockerfile                       # Multi-stage build (golang → alpine)
├── .gitignore
├── CLAUDE.md                        # AI agent context
├── README.md
│
├── server/
│   ├── database.go                  # PostgreSQL connection, AutoMigrate
│   ├── routes.go                    # Route registration, middleware setup
│   ├── mainTest.go                  # Test entrypoint
│   │
│   ├── middleware/
│   │   └── auth.go                  # JWT generation & validation middleware
│   │
│   ├── data_models/
│   │   ├── user.go                  # User model + CRUD + bcrypt password hashing
│   │   ├── role.go                  # Role model + SeedRoles()
│   │   ├── message.go               # Message model + CRUD
│   │   ├── game_score.go            # GameScore model + JSONB metadata + leaderboard
│   │   ├── ludde_feeding_times.go   # LuddeFeedingTime model + CRUD
│   │   └── template.go             # Template/example CRUD model
│   │
│   ├── data_functions/
│   │   ├── utils.go                 # GitCommit var, S3 session init, S3 fetch helpers
│   │   ├── s3_functions.go          # S3 list objects, upload
│   │   ├── word_weaver_scripts.go   # Dictionary loading, word validation, board processing
│   │   ├── word_weaver_scripts_test.go
│   │   └── utils_test.go
│   │
│   ├── handlers/
│   │   ├── common.go                # ErrorResponse, SuccessResponse, ValidateRole()
│   │   ├── login.go                 # Login handler (JWT generation)
│   │   ├── user.go                  # User CRUD, Bootstrap admin
│   │   ├── message.go               # Message CRUD
│   │   ├── feeding.go               # Feeding time CRUD
│   │   ├── game.go                  # Word Weaver + game score handlers
│   │   ├── misc.go                  # Health check, S3 fetch, image upload, asset list
│   │   ├── files.go                 # Full file management (list/upload/download/delete/folders)
│   │   ├── voice.go                 # Transcriber proxy handlers
│   │   ├── voice_test.go            # Contract tests for transcriber proxy
│   │   ├── commander.go             # Commander proxy handlers
│   │   ├── translator.go            # Translator proxy handlers
│   │   └── asset_mappings.go        # Asset mapping CRUD with defaults
│   │
│   └── utils/
│       └── create_dictionary.py     # Offline dictionary builder script
│
├── docs/
│   ├── SERVER_SPEC.md
│   ├── SERVER_ARCHITECTURE.md
│   ├── SERVER_OPERATIONS.md
│   ├── CHANGELOG.md
│   └── DECISIONS.md
│
└── .github/
    └── workflows/
        └── ci.yml
```

## Request Flow

1. **HTTP request** arrives at Gin engine on port 8080
2. **CORS middleware** allows cross-origin requests
3. **Custom logger** formats request logs with role/user info
4. **JWT middleware** (for protected routes) extracts and validates token
5. **Role validation** (for trusted/admin routes) checks `role_name` claim
6. **Handler** processes the request:
   - Direct DB access via GORM for data model operations
   - S3 SDK calls for asset/file operations
   - HTTP proxy to microservices for transcriber/commander/translator
7. **Response** returned with appropriate status code and content type

## Authentication Flow

```
Client                Server                  Database
  │                     │                        │
  │  POST /login        │                        │
  │  {user, password}   │                        │
  │────────────────────>│                        │
  │                     │  Lookup user by name   │
  │                     │───────────────────────>│
  │                     │  User + hashed pwd     │
  │                     │<───────────────────────│
  │                     │                        │
  │                     │  bcrypt.Compare()      │
  │                     │  GenerateToken()       │
  │                     │  (HS256, 24h expiry)   │
  │                     │                        │
  │  200 { token }      │                        │
  │<────────────────────│                        │
  │                     │                        │
  │  GET /files/list    │                        │
  │  Auth: Bearer <jwt> │                        │
  │────────────────────>│                        │
  │                     │  Validate JWT          │
  │                     │  Extract claims:       │
  │                     │    user_id, role_name  │
  │                     │  Check role >= Trusted │
  │                     │                        │
  │  200 [files...]     │                        │
  │<────────────────────│                        │
```

## Proxy Pattern

For transcriber, commander, and translator, the server acts as a transparent reverse proxy:

```
Client ──> aspirant-server ──> microservice
                │                    │
                │  Forward request   │
                │  (body + headers)  │
                │───────────────────>│
                │                    │
                │  Pipe response     │
                │  (status + body)   │
                │<───────────────────│
                │                    │
Client <── aspirant-server <── microservice
```

Each proxy has a configurable URL via environment variable with a sensible Docker-network default.
