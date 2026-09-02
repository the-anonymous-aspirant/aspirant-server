# Changelog

## Unreleased

### Changed

- **Constellations: a player now sees only the connections they are part of.**
  `GET .../rooms/:code/state` shipped the room's whole relationship graph to
  every member, so anyone could read who else was connected to whom — from the
  board, or straight out of devtools. The `relationships` array is now scoped to
  the caller: an edge is serialized only to the two players it joins. The same
  filter is applied to `GET .../rooms/:code/relationships`, which returned the
  whole graph to any member as well and would otherwise have stayed open behind
  a front door that looked fixed. Nothing else narrows — members, occupancy,
  dice and history stay room-wide, so the board still shows every player and
  only the lines between them are private.

  Endpoint-scoped, not author-scoped: the connection graph stays shared and
  single (one active edge per unordered pair), and `RoomRelationships` still
  reads it whole, because goal-achievement detection has to evaluate victory
  conditions over edges between other players while each client sees less than
  all of them. Privacy is a property of the serializer, so revealing the graph
  later — at game end, say — is a one-line change rather than a rework.
  Operator finding, system_3 #4806 ask 2.

- **Constellations: a join refusal now says which condition blocked it.** Every
  refusal from `POST /api/constellations/rooms/:code/join` (and from room
  create) carries an additive `reason` on the error detail —
  `room_not_found`, `room_ended`, `room_full`, `already_in_game`,
  `not_in_room`, `invalid_player_count`. `error.code` is derived from the HTTP
  status, so a full room and a caller already seated elsewhere were both
  `conflict`: a client could only tell them apart by string-matching the human
  message. Two refusals now carry the detail needed to explain themselves:
  `already_in_game` keeps the `active_room_code` from #4798, and `room_full`
  gains `room_player_count` plus a message naming the room and its size. A code
  whose game has *ended* is also no longer reported as an unknown code — it
  stays a 404 but answers `room_ended` / "That game has ended", via a new
  `data_models.ErrRoomEnded` and `EndedRoomExists`. `code`, `message` and
  `active_room_code` are unchanged, so existing readers are unaffected. This is
  the server half of the scanned-link auto-join; the client half is system_3
  #4810. Operator finding, system_3 #4806 ask 1.

### Fixed

- **Constellations: the already-in-a-game refusal now names the room.** Create
  and join answered `ErrAlreadyInGame` with a bare `"You are already in an
  active game"`, which told the user nothing they could act on — they could not
  tell which room held their seat, so they could not navigate there to leave it
  and free themselves. The 409 now looks the caller's active room up
  (`data_models.ActiveRoomForUser`) and names it in both the message ("You are
  already in game ZK4TQ — leave it before starting or joining another.") and an
  additive `active_room_code` field on the error detail, which the client
  renders as a link to that room. `code` and `message` keep their existing
  shape, and the field is omitted (message falling back to the bare form) if
  the lookup finds no room. Operator finding, system_3 #4798.

- **Constellations: a solo creator's room no longer dies when they step out.**
  The slate-on-empty rule fired whenever the last in-room member left; for a
  room whose only member was its creator, one Leave — or, since #4778, one
  logout — emptied and slated it, so anyone opening the shared code got a 404.
  Rooms now slate on empty only once they have been *played*: a new
  `Room.EverHadTwoMembers` latch is set in `JoinRoom` the moment occupancy first
  reaches two, and both `LeaveRoom` and `LeaveAllActiveRooms` slate through a
  shared `maybeSlate` helper that requires it. A never-played room stays active
  on empty so its code remains joinable (reaping truly abandoned rooms is a
  separate TTL concern). Operator finding, system_3 #4785.

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
