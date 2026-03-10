# Changelog

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
