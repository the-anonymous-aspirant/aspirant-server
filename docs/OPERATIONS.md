# Server Operations

## Setup

### Prerequisites

- Go 1.16+ (module uses `aspirant-online` as module name -- no refactoring needed)
- PostgreSQL (any recent version)
- AWS credentials with S3 read/write access
- Docker (for containerized deployment)

### Environment Configuration

Create a `.env` file in the project root:

```env
DB_HOST=localhost
DB_USER=postgres
DB_PASSWORD=your_password
DB_NAME=aspirant
DB_PORT=5432

AWS_ACCESS_KEY_ID=your_key
AWS_SECRET_ACCESS_KEY=your_secret
AWS_REGION=eu-central-1
S3_BUCKET_NAME=your_bucket

# Optional: microservice URLs (defaults to Docker network names)
TRANSCRIBER_URL=http://localhost:8001
COMMANDER_URL=http://localhost:8002
TRANSLATOR_URL=http://localhost:8003

# Optional: Gin mode (debug/release)
GIN_MODE=debug
```

### First Run

1. Start PostgreSQL
2. Configure `.env`
3. Run the server: `go run main.go`
4. Bootstrap the first admin user:
   ```bash
   curl -X POST http://localhost:8080/bootstrap/admin \
     -H "Content-Type: application/json" \
     -d '{"user_name": "admin", "password": "your_password"}'
   ```
5. The server auto-migrates all database tables on startup

## Running

### Local Development

```bash
# Direct run
go run main.go

# Build and run
go build -o main . && ./main

# With git commit injection
go build -ldflags "-X aspirant-online/server/data_functions.GitCommit=$(git rev-parse --short HEAD)" -o main .
```

### Docker

```bash
# Build
docker build \
  --build-arg DB_USER=postgres \
  --build-arg DB_HOST=db \
  --build-arg DB_PASSWORD=secret \
  --build-arg DB_NAME=aspirant \
  --build-arg DB_PORT=5432 \
  --build-arg AWS_ACCESS_KEY_ID=... \
  --build-arg AWS_SECRET_ACCESS_KEY=... \
  --build-arg AWS_REGION=eu-central-1 \
  --build-arg S3_BUCKET_NAME=... \
  --build-arg GIT_COMMIT=$(git rev-parse --short HEAD) \
  -t aspirant-server .

# Run
docker run -p 8080:8080 aspirant-server
```

## Testing

```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run specific package tests
go test -v ./server/data_functions/...
go test -v ./server/handlers/...
```

## Debugging

### Health Check

```bash
curl http://localhost:8080/health
```

Returns git commit hash and server status.

### Common Issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| `Error opening database` | PostgreSQL not running or wrong credentials | Check DB_HOST, DB_USER, DB_PASSWORD, DB_NAME in .env |
| `Error initializing S3 session` | Missing AWS credentials | Set AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION |
| `Translator service unavailable` | Translator container not running | Start translator or set TRANSLATOR_URL |
| `Transcriber service unavailable` | Transcriber container not running | Start transcriber or set TRANSCRIBER_URL |
| `Commander service unavailable` | Commander container not running | Start commander or set COMMANDER_URL |
| Server starts but DB routes fail | DB connection failed (non-fatal) | Check logs for "WARNING: Database connection failed" |

### Logging

The server uses a custom Gin log format that includes:
- Timestamp
- HTTP status code
- Client IP
- HTTP method and path
- User role (from JWT claims)
- User ID and username

Set `GIN_MODE=debug` for verbose logging, `GIN_MODE=release` for production.

## How to Add a New Proxy Route

To proxy a new microservice endpoint:

1. **Create or extend a handler file** in `server/handlers/`:
   ```go
   func NewServiceProxyGet(c *gin.Context, path string) {
       url := fmt.Sprintf("%s%s", serviceURL(), path)
       resp, err := serviceClient.Get(url)
       // ... standard proxy pattern (see translator.go for reference)
   }
   ```

2. **Add the route** in `server/routes.go` under the appropriate group:
   ```go
   adminRoutes.GET("/new-service/endpoint", handlers.NewServiceHandler)
   ```

3. **Add environment variable** for the service URL with a default:
   ```go
   func serviceURL() string {
       if url := os.Getenv("NEW_SERVICE_URL"); url != "" {
           return url
       }
       return "http://new-service:8000"
   }
   ```

## How to Add a New Data Model

1. **Create model file** in `server/data_models/`:
   ```go
   type NewModel struct {
       ID        uint      `json:"id" gorm:"primary_key"`
       CreatedAt time.Time `json:"created_at"`
       // ... fields
   }
   ```

2. **Add AutoMigrate** call in `server/database.go`:
   ```go
   db.AutoMigrate(&data_models.NewModel{})
   ```

3. **Create handler file** in `server/handlers/`

4. **Register routes** in `server/routes.go`
