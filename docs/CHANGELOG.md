# Changelog

## Unreleased

### Fixed

- **Constellations: logging out now leaves your active game.** Membership was
  tracked by a `constellation_room_members` row with `left_at` NULL, and
  create/join refuse a user who holds one (`ErrAlreadyInGame`, the
  one-game-at-a-time lock). Logout cleared only the auth cookie, so the row
  lingered and stranded the user out of *both* create and join on every future
  login until the room slated from another player's side. `LogoutHandler` now
  recovers the user from the still-valid token and calls the new
  `data_models.LeaveAllActiveRooms`, which leaves every active membership and
  slates any room that empties — the same lifecycle `LeaveRoom` applies, keyed
  on the user. Best-effort and idempotent: an anonymous or game-less logout is
  a no-op and never blocks the cookie clear. Operator finding, system_3 #4778.

### Security

- **User routes no longer disclose `access_role` to non-Admins.** `PublicUserResponse` (the DTO served to non-Admin callers of the user list/by-id routes) dropped its `access_role` field, so an authenticated non-Admin can no longer enumerate which account is the Admin (CWE-639 / OWASP A01 — security-finding #3093). The `email`/`comment` Admin gate from #1380 is unchanged. The item route (`GET /data_models/users/:id`) now returns the same `PublicUserResponse` for a non-Admin cross-id lookup instead of a `403`, so it and the collection route tell the same story — the former `403` protected nothing the collection route did not already list.
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
