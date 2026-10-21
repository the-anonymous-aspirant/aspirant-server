# Changelog

## Unreleased

### Added

- **Invoicing app: Excel workbook export** (system_3 #8367/#8377).
  `GET /api/invoicing/export/workbook.xlsx` returns one `Rechnungen.xlsx`
  with four sheets: Invoices, Line items, Payments (with category, client and
  bank deposit) and Clients. Amounts are numeric cells. Every text cell is an
  inline string, which Excel and LibreOffice never evaluate as a formula, and
  also gets the `'` guard. Like the CSV exports, it runs the §5 validation
  first and answers 409 with the report while there are violations. The file
  is written with the Go standard library (`archive/zip`): four plain tables
  need no spreadsheet dependency. An imported PayPal row whose currency is not
  a three-letter ISO code now fails the import, naming its line (security N6).
- **Invoicing app: §5 validation and CSV exports** (system_3 #8367/#8374).
  `GET /api/invoicing/validation` re-checks the stored history and reports
  each finding with a code:
  - violations: gaps, duplicates or out-of-order numbers under a prefix
    (a sequence may start above 1); future issue dates; a service date after
    the issue date; totals that differ from the line items; a payment that is
    not paid work, or that sits on two issued invoices; an issued invoice
    without its PDF;
  - warning only: an invoice total that differs from its linked payments.

  `GET /api/invoicing/invoices/:id/export.csv` (one row per line item) and
  `GET /api/invoicing/export/invoices.csv` (one row per invoice) produce
  German-Excel CSVs: UTF-8 BOM, `;` as the separator, decimal comma. Both run
  the validation first and answer 409 with the report while it has
  violations. Every free-text cell that starts with `=`, `+`, `-`, `@`, a tab
  or a CR is prefixed with `'`, so text written by an outside payer is never
  run as a spreadsheet formula. Each export writes an audit row.
- **Invoicing app: an issue is refused when the PDF cannot print its text**
  (system_3 #8367, security N5). The PDF library silently drops any character
  the embedded font has no glyph for, which would print a name with letters
  missing. Issuing now answers 422 `unprintable_characters`, naming the
  characters, and uses no number. Line breaks and tabs in a description are
  laid out as line breaks and spaces, not reported as missing characters.
- **Invoicing app: German invoice PDF** (system_3 #8367/#8373). Issuing an
  invoice now renders its A4 PDF from the snapshots frozen at issue and stores
  it in `invoicing_invoice_pdfs`, in the same transaction. A render failure
  rolls the issue back, so an issued invoice always has its document. The PDF
  carries every spec §4.5 field: the heading "Rechnung", the issuer with the
  Steuernummer, the client, the number, the Rechnungsdatum, the Leistungsdatum
  or Leistungszeitraum, and the line items with totals. It also carries either
  the § 19 UStG sentence or the net, VAT and gross amounts, one payment
  confirmation per linked payment, and the retroactive-issue sentence, all
  verbatim from the spec. `GET /api/invoicing/invoices/:id/pdf` serves the
  stored bytes (`Rechnung-<number>.pdf`, with an `X-Content-SHA256` header);
  a draft returns 409. Rendering uses gopdf with embedded Liberation Sans (see
  DECISIONS.md).
- **Invoicing app: drafts, issuing, lock and Storno** (system_3 #8367/#8371).
  `POST /api/invoicing/invoices` starts a draft from paid-work payments,
  prefilled only from data: one line per payment, the client when all payments
  share it, and the payment dates as the service period. Fields the data does
  not supply stay empty. Drafts are edited with `PUT` and removed with
  `DELETE`. `POST /api/invoicing/invoices/:id/issue` is the only place a number
  and a date are set, and both come from the server: the next number under the
  profile's prefix (`2026-001`), drawn from a counter row updated inside the
  issuing transaction so numbers stay unique and gap-free, and today's date in
  Europe/Berlin. Issuing also freezes snapshots of the profile and the client.
  It refuses, with a reason code, an incomplete profile (including an empty
  Steuernummer), an incomplete client, incomplete line items, a missing or
  future service date, a payment that is not paid work, a payment already on
  another issued invoice, and a clock behind the previous issue date. A body
  naming `issue_date`, `invoice_number` or any other server-owned field is
  refused by name. Issued invoices refuse every edit and are never deleted;
  `POST /api/invoicing/invoices/:id/cancel` (Storno) keeps them on record as
  cancelled and frees their payments for a corrected invoice. Once a number
  has been issued under a prefix, the profile can no longer change that
  prefix's next number, and an invoiced payment's category is locked.
  `GET /api/invoicing/invoices` lists invoices, filtered by status, issue
  date, client and client-name search.
- **Invoicing app: PayPal import and payment categorisation** (system_3
  #8367/#8370). `POST /api/invoicing/payments/import` takes a PayPal activity
  CSV (English or German headers, comma/semicolon/tab, UTF-8 or
  Windows-1252, localised amounts parsed to integer cents) and stores the
  incoming payments plus withdrawals to the bank as deposit references. The
  `(source, transaction id)` pair is unique, so re-uploading a file imports
  nothing new. Fees, currency conversions, bank top-ups, holds, pending and
  outgoing rows are skipped, and the summary counts each skip by reason. A file
  whose `NN/NN/YYYY` dates read both ways is refused unless `date_order` says
  which order applies; the import never guesses. A new payment whose sender
  matches a client's PayPal email is linked to that client.
  `GET /api/invoicing/payments` lists payments, and
  `PATCH /api/invoicing/payments/:id` sets the category (uncategorised,
  paid_work, gift, loan, refund or other), the client and the bank deposit, with
  an audit row for each change.
- **Invoicing app: issuer profile, clients and audit trail** (system_3
  #8367/#8369). The first slice of the retroactive-invoice app for flowah.
  `GET/PUT /api/invoicing/profile` holds the issuer header (name, address,
  Steuernummer, email, § 19 UStG status, number prefix and next number);
  `GET/POST /api/invoicing/clients` and `PUT /api/invoicing/clients/:id`
  manage clients, who need a name and an address; and
  `GET /api/invoicing/audit-events` reads the append-only trail. Every write
  records its audit row in the same transaction. The whole `/invoicing` group
  sits behind a flowah + Admin gate, and a test walks every route registered
  under it, so a route added later cannot skip the gate. The tables are
  `invoicing_*` and share nothing with the retired finance service.
- **Pappas sit-ups log** (system_3 #7992/#7993). `GET /api/situps/entries`
  returns one row per day from 2026-10-18 to 2026-12-13 (57 days,
  gap-filled), and `PATCH /api/situps/entries/:date` upserts a day's count
  inside that window and inside today ± 2 days. Both routes sit behind the
  same robert + Admin gate as the pushups log. It is robert's own log, so it
  uses a Date-keyed `situp_entries` table and a route gate, the single-tenant
  shape #4203 chose for pushups, not a per-user schema. There is no milestone
  table; the client decides sit-up messages and crowns.

### Changed

- **Public sign-up can no longer be opened on a deployment that cannot deliver
  mail.** Sign-up completes only through an emailed verification link and
  `LoginHandler` refuses an unverified account, so opening the `#5289`
  kill-switch while the mail sender is the dev sink manufactured accounts that
  could never log in — and told the visitor only "Invalid login credentials".
  That is not hypothetical: it is the state the operator found by hand on
  2026-09-29 after creating an account on the deployed server and receiving no
  mail (system_3 #6742, fixed durably here as #6764).

  `PUT /api/settings/signup` with `enabled=true` now answers `409` when the
  process holds a non-delivering sender, and the refusal names the four
  variables `email.SenderFromEnv` actually reads — `SMTP_HOST`,
  `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`. The names are the trap worth
  spelling out: the deployment's compose gives this service `env_file: .env`,
  which supplies `SMTP_USER`, a variable this package deliberately does not
  read, and supplies no `SMTP_FROM` at all, so a plausible-looking
  provisioning attempt leaves a partial configuration that logs an error and
  keeps the sink. CLOSING sign-up is never gated — the switch that removes the
  trap has to work on a deployment in any state.

  `SignupHandler` carries the same refusal, immediately after the kill-switch
  and before the users table is touched, because the flag can read true from
  before a relay was removed or from a direct row write that never passed
  through the admin endpoint. It answers with the existing closed-site message
  rather than one of its own: from an anonymous caller's side a site that
  cannot complete a sign-up is closed, and a distinct "our mail is broken"
  reply would publish the deployment's mail state to anyone who asked. The
  admin-facing reason stays on the toggle, behind the admin tier. Verification
  of a token already issued is unaffected — finishing an existing sign-up
  sends no mail.

  The predicate is `email.Delivers(Sender)`, a type test for the dev sink
  rather than a second reading of the environment or a boolean threaded
  through the request context. `SenderFromEnv` already decides this — it
  returns a `LogSender` for all three non-delivering outcomes and an
  `SMTPSender` only for a complete relay — so the sender the handlers already
  hold is the evidence. A context flag would have needed a default for the key
  being absent, and both defaults are defects: "delivers" makes a forgotten
  wiring line a silent no-op guard, "does not deliver" closes sign-up
  site-wide. The test drives the real `SenderFromEnv` across every
  configuration path and asserts `Delivers` agrees with the `sends` boolean
  the boot line already reports, so the guard cannot drift from the log.

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
