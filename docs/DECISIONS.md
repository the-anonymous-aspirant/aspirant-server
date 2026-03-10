# Decisions

Key architectural and design decisions for aspirant-server.

## Go + Gin

**Decision:** Use Go with the Gin web framework.

**Rationale:** Gin provides a lightweight, high-performance HTTP framework with built-in middleware support, route grouping, and JSON binding. Go's single-binary compilation simplifies Docker builds and deployment.

## GORM with Auto-Migrate

**Decision:** Use GORM ORM with automatic schema migration on startup.

**Rationale:** Auto-migrate ensures the database schema stays in sync with Go structs without requiring manual migration scripts. This is acceptable for a small-scale personal project where downtime during schema changes is not a concern. The server includes a legacy migration path (access_role to role_id) that runs once and then becomes a no-op.

## JWT with 24-Hour Expiry

**Decision:** Use HS256-signed JWTs with a 24-hour expiration.

**Rationale:** JWT tokens are stateless and do not require server-side session storage. The 24-hour expiry balances security with usability -- users stay logged in for a day without re-authenticating. The signing secret is hardcoded for simplicity (acceptable for a personal project, not for production at scale).

## Reverse Proxy Pattern for Microservices

**Decision:** The server proxies requests to transcriber, commander, and translator services rather than clients calling them directly.

**Rationale:** Centralizing API access through a single gateway provides:
- Unified authentication and authorization (microservices trust the gateway)
- Single CORS origin for the frontend
- Service discovery via environment variables
- Ability to add/remove microservices without changing the frontend

## Multi-Stage Docker Build

**Decision:** Use a two-stage Docker build (Go builder + Alpine production).

**Rationale:** The Go builder stage compiles a static binary. The Alpine production stage contains only the binary and CA certificates, resulting in a minimal container image (~20 MB vs ~1 GB for a full Go image). Build arguments inject environment variables and the git commit hash.

## Module Name Retained as aspirant-online

**Decision:** Keep the Go module name as `aspirant-online` rather than renaming to `aspirant-server`.

**Rationale:** Renaming the module would require updating every import path across all Go files. Since this is a standalone extraction and not a published library, the module name is an internal detail that does not affect external consumers.

## Local Filesystem for File Storage

**Decision:** Store user files on the local filesystem at `/data/files/` rather than S3.

**Rationale:** Local storage is simpler for a personal project and avoids S3 costs for user-uploaded content. S3 is still used for shared assets (images, audio, dictionary) that need CDN-like access patterns. The 50 GB per-user quota prevents unbounded disk usage.

## Non-Fatal Database Connection

**Decision:** The server starts even if the database connection fails.

**Rationale:** This allows the server to serve S3 assets, health checks, and static content even when PostgreSQL is unavailable. DB-dependent routes will return errors, but the server remains accessible for debugging and asset delivery.
