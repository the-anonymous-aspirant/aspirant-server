# Changelog

## Unreleased

### Security

- **JWT signing hardened.** `JWT_SECRET` is now required at boot; empty, known-placeholder (`change-me` / `aspirant_secret` / `aspirant_secret_CHANGE_ME`), and short (<32 byte) values fatally reject startup. The token parser pins HS256 via `jwt.WithValidMethods([]string{"HS256"})` and the keyfunc additionally asserts `*jwt.SigningMethodHMAC`, closing alg-none and alg-confusion. Migrated off the unmaintained `dgrijalva/jwt-go v3.2.0` (CVE-2020-26160) to `github.com/golang-jwt/jwt/v5 v5.3.1`; `iat` and `nbf` are validated in addition to `exp`. See system_3 #1374.

## v1.0.0 -- Initial Release

Extracted from the [aspirant-online](https://github.com/the-anonymous-aspirant/aspirant-online) monorepo into a standalone repository.

### Included

- Go/Gin HTTP API gateway with JWT authentication and RBAC
- PostgreSQL/GORM data models: users, roles, messages, game_scores, ludde_feeding_times
- File management system with per-user and shared storage (50 GB quotas)
- Word Weaver game logic with S3-backed dictionary
- S3 asset management with semantic name-to-hash mappings
- Reverse proxy to microservices: transcriber, commander, translator
- Multi-stage Docker build (golang:1.23.4 builder, alpine production)
- GitHub Actions CI workflow
- Documentation: spec, architecture, operations, decisions
