# Easter Egg Hunt — Spec

## Motivation

A shared, time-limited Easter egg hunt game for all platform users. Players reveal squares on a 32x32 grid to find hidden eggs. The game locks on Easter Saturday evening, creating a natural deadline and competitive window.

## Scope

New feature on existing services — **not** a new microservice:
- **aspirant-server:** New GORM models, handlers, and routes under `/games/easter-hunt`
- **aspirant-client:** New Vue page at `/easter-hunt`

---

## Data Model

Three new tables, all owned by aspirant-server. Auto-migrated via GORM.

### `easter_hunt_games`

The current game instance. Only one active game at a time.

| Column | Type | Constraints | Description |
|--------|------|-------------|-------------|
| `id` | `uint` | PK, auto-increment | gorm.Model |
| `seed` | `int64` | not null | Deterministic seed for egg placement |
| `lock_at` | `timestamp` | not null | When the game stops accepting clicks |
| `is_active` | `bool` | not null, default true | Only one game can be active |
| `created_at` | `timestamp` | auto | gorm.Model |
| `updated_at` | `timestamp` | auto | gorm.Model |
| `deleted_at` | `timestamp` | soft delete | gorm.Model |

```go
type EasterHuntGame struct {
    gorm.Model
    Seed     int64     `json:"seed" gorm:"not null"`
    LockAt   time.Time `json:"lock_at" gorm:"not null"`
    IsActive bool      `json:"is_active" gorm:"not null;default:true"`
}
```

### `easter_hunt_clicks`

Every square reveal. One row per click.

| Column | Type | Constraints | Description |
|--------|------|-------------|-------------|
| `id` | `uint` | PK, auto-increment | gorm.Model |
| `game_id` | `uint` | not null, index, FK → easter_hunt_games | Which game |
| `user_id` | `uint` | not null, index | Who clicked |
| `x` | `int` | not null | Column (0-31) |
| `y` | `int` | not null | Row (0-31) |
| `created_at` | `timestamp` | auto | When the click happened |
| `updated_at` | `timestamp` | auto | gorm.Model |
| `deleted_at` | `timestamp` | soft delete | gorm.Model |

Unique constraint on `(game_id, x, y)` — each square can only be revealed once.

```go
type EasterHuntClick struct {
    gorm.Model
    GameID uint `json:"game_id" gorm:"not null;index;uniqueIndex:idx_game_xy"`
    UserID uint `json:"user_id" gorm:"not null;index"`
    X      int  `json:"x" gorm:"not null;uniqueIndex:idx_game_xy"`
    Y      int  `json:"y" gorm:"not null;uniqueIndex:idx_game_xy"`
}
```

### `easter_hunt_scores`

Denormalized score tracking. One row per user per game. Updated when a user completes an egg.

| Column | Type | Constraints | Description |
|--------|------|-------------|-------------|
| `id` | `uint` | PK, auto-increment | gorm.Model |
| `game_id` | `uint` | not null, index, FK → easter_hunt_games | Which game |
| `user_id` | `uint` | not null, index | Who scored |
| `score` | `int` | not null, default 0 | Number of eggs completed |
| `created_at` | `timestamp` | auto | gorm.Model |
| `updated_at` | `timestamp` | auto | gorm.Model |
| `deleted_at` | `timestamp` | soft delete | gorm.Model |

Unique constraint on `(game_id, user_id)`.

```go
type EasterHuntScore struct {
    gorm.Model
    GameID uint `json:"game_id" gorm:"not null;uniqueIndex:idx_game_user"`
    UserID uint `json:"user_id" gorm:"not null;uniqueIndex:idx_game_user"`
    Score  int  `json:"score" gorm:"not null;default:0"`
}
```

---

## Egg Placement Algorithm

### Requirements
- 24 eggs on a 32x32 grid (1,024 squares total, 384 occupied by eggs)
- Each egg is 16 connected squares shaped like an oval/egg
- Eggs must not overlap
- Deterministic from seed — same seed always produces the same layout

### Algorithm

1. Initialize a PRNG with the game's `seed`
2. Define a canonical egg shape as a set of 16 relative offsets forming an egg silhouette. Three shape variants (tall, wide, tilted) provide visual variety:

```
Variant A (tall):       Variant B (wide):       Variant C (tilted):
    . X X .                 . . . .                 . X X .
    X X X X               . X X X X .              X X X .
    X X X X               X X X X X X              X X X X
    X X X X               . X X X X .              . X X X
    . X X .                 . . . .                 . X X .
```

Each variant is stored as a list of 16 `(dx, dy)` offsets from a top-left anchor.

3. For each of the 24 eggs:
   a. Pick a variant using the PRNG (index mod 3)
   b. Pick a random anchor position `(ax, ay)` where `0 <= ax <= 31 - width` and `0 <= ay <= 31 - height`
   c. Compute the 16 absolute positions by adding offsets to the anchor
   d. Check that none of the 16 positions are already occupied
   e. If collision, retry with a new random position (max 100 attempts per egg)
   f. Mark all 16 positions as occupied and record the egg ID for each

4. Return a map: `position (x,y) → egg_id (0-23)` for occupied squares, and `-1` for empty squares

The server computes this once per game and caches the result in memory. The egg map is never sent to the client — only revealed squares are returned.

### Egg Colors

Each egg gets a color assigned deterministically from the seed. Use a palette of 8 vivid colors, assigned round-robin with a PRNG shuffle:

```
#FF4057 (red), #FF6B35 (orange), #FFD23F (yellow), #06D6A0 (green),
#118AB2 (blue), #7B2FF7 (purple), #F72585 (magenta), #2EC4B6 (teal)
```

---

## API Endpoints

All endpoints under `/games/easter-hunt`. Follow existing server conventions (RespondWithError, RespondWithSuccess, PaginatedResponse).

### Public Endpoints

#### `GET /games/easter-hunt/state`

Returns the current game board state. No authentication required (allows spectators).

**Response (200):**
```json
{
  "status": "success",
  "data": {
    "game_id": 1,
    "lock_at": "2026-04-05T16:00:00Z",
    "is_locked": false,
    "board": {
      "width": 32,
      "height": 32,
      "revealed": [
        {"x": 5, "y": 10, "egg_id": -1, "user_id": 3, "username": "alice"},
        {"x": 12, "y": 7, "egg_id": 2, "user_id": 5, "username": "bob"}
      ],
      "eggs": [
        {"egg_id": 0, "color": "#FF4057", "squares": 16, "revealed": 9, "completed": false, "completed_by": null},
        {"egg_id": 1, "color": "#FF6B35", "squares": 16, "revealed": 16, "completed": true, "completed_by": "alice"}
      ]
    },
    "scores": [
      {"user_id": 3, "username": "alice", "score": 2},
      {"user_id": 5, "username": "bob", "score": 1}
    ]
  },
  "message": "Game state retrieved"
}
```

Notes:
- `revealed` contains only clicked squares (not the full 1,024)
- `egg_id: -1` means the square is empty (no egg underneath)
- `eggs` only includes eggs that have at least 1 revealed square (don't leak unstarted egg positions)
- `scores` ordered by score descending, then by earliest first score
- `lock_at` is always UTC (CEST offset handled client-side)

**Response when no active game (404):**
```json
{
  "error": {"code": "not_found", "message": "No active game"}
}
```

#### `GET /games/easter-hunt/scores`

Dedicated scoreboard endpoint with pagination.

**Query params:** `page` (default 1), `page_size` (default 20, max 100)

**Response (200):**
```json
{
  "items": [
    {"user_id": 3, "username": "alice", "score": 2},
    {"user_id": 5, "username": "bob", "score": 1}
  ],
  "total": 5,
  "page": 1,
  "page_size": 20
}
```

### Authenticated Endpoints

#### `POST /games/easter-hunt/clicks`

Reveal a square. Requires authentication.

**Request body:**
```json
{"x": 12, "y": 7}
```

**Validation:**
- `x` and `y` must be integers in range [0, 31]
- Game must be active and not locked (current time < `lock_at`)
- User must not be on cooldown (last click must be >= 5 minutes ago)
- Square must not already be revealed

**Response — success (200):**
```json
{
  "status": "success",
  "data": {
    "x": 12,
    "y": 7,
    "egg_id": 2,
    "egg_completed": true,
    "scored": true,
    "cooldown_until": "2026-04-03T14:30:00Z",
    "next_click_seconds": 300
  },
  "message": "Square revealed"
}
```

- `egg_id`: ID of the egg at this position, or `-1` if empty
- `egg_completed`: true if this click revealed the last square of an egg
- `scored`: true if this user gets the point (same as `egg_completed`)
- `cooldown_until`: UTC timestamp when the user can click again
- `next_click_seconds`: always 300 (5 minutes), for client convenience

**Response — on cooldown (429):**
```json
{
  "error": {
    "code": "cooldown",
    "message": "You can click again in 3m 42s",
    "details": {"cooldown_until": "2026-04-03T14:30:00Z", "remaining_seconds": 222}
  }
}
```

**Response — game locked (403):**
```json
{
  "error": {"code": "game_locked", "message": "The hunt has ended"}
}
```

**Response — square already revealed (409):**
```json
{
  "error": {"code": "conflict", "message": "Square already revealed"}
}
```

**Response — invalid coordinates (400):**
```json
{
  "error": {"code": "validation_error", "message": "x and y must be between 0 and 31"}
}
```

#### `GET /games/easter-hunt/cooldown`

Check the user's current cooldown status. Useful for the client to show the countdown timer without attempting a click.

**Response (200):**
```json
{
  "status": "success",
  "data": {
    "on_cooldown": true,
    "cooldown_until": "2026-04-03T14:30:00Z",
    "remaining_seconds": 222
  },
  "message": "Cooldown status retrieved"
}
```

### Admin Endpoints

Require authentication + Admin role.

#### `POST /games/easter-hunt/admin/reset`

Reset the game. Deletes all clicks and scores for the active game, generates a new seed, recomputes egg positions. If no active game exists, creates one.

**Request body (optional):**
```json
{"lock_at": "2026-04-05T16:00:00Z"}
```

If `lock_at` is omitted, defaults to `2026-04-05T16:00:00Z` (18:00 CEST = 16:00 UTC).

**Response (200):**
```json
{
  "status": "success",
  "data": {
    "game_id": 2,
    "seed": 1743500000,
    "lock_at": "2026-04-05T16:00:00Z",
    "egg_count": 24
  },
  "message": "Game reset"
}
```

#### `GET /games/easter-hunt/admin/reveal`

Returns the full board with all egg positions. For admin debugging — does not affect the actual game state visible to other users.

**Response (200):**
```json
{
  "status": "success",
  "data": {
    "game_id": 1,
    "seed": 1743500000,
    "eggs": [
      {"egg_id": 0, "color": "#FF4057", "squares": [{"x": 5, "y": 3}, {"x": 5, "y": 4}, ...]},
      {"egg_id": 1, "color": "#FF6B35", "squares": [{"x": 14, "y": 9}, ...]}
    ]
  },
  "message": "Full board revealed"
}
```

---

## Cooldown Logic

- Tracked per user by querying the most recent `easter_hunt_clicks` row for that user and game
- Cooldown is 5 minutes (300 seconds) from the `created_at` of the last click
- No separate cooldown table needed — derive from clicks
- Server compares `time.Now().UTC()` against `lastClick.CreatedAt.Add(5 * time.Minute)`
- If on cooldown, return 429 with remaining seconds

---

## Scoring Logic

When a click reveals a square that belongs to an egg:
1. Count how many of that egg's 16 squares are now revealed (including this click)
2. If count == 16, the egg is complete
3. Upsert the clicking user's `easter_hunt_scores` row: increment `score` by 1
4. The response indicates `egg_completed: true` and `scored: true`

Only the user who reveals the **final** square gets the point. Partial reveals give no score.

---

## Frontend — Vue Page

### Route

```javascript
{ path: '/easter-hunt', component: EasterHuntView, meta: {} }
```

No role restriction — the page is visible to everyone. Unauthenticated users can view but not click.

### Layout

```
┌────────────────────────────────────────────────────────────┐
│  Easter Egg Hunt                              [time left]  │
├──────────────────────────────────┬─────────────────────────┤
│                                  │  Scoreboard             │
│                                  │  ─────────              │
│       32×32 Game Board           │  1. alice — 3 eggs      │
│                                  │  2. bob   — 2 eggs      │
│      (canvas element)            │  3. carol — 1 egg       │
│                                  │                         │
│                                  │  ─────────              │
│                                  │  Your cooldown: 2:34    │
│                                  │                         │
│                                  │  [Admin: Reset]         │
│                                  │  [Admin: Reveal]        │
├──────────────────────────────────┴─────────────────────────┤
│  Game ends: April 5, 18:00 CEST                           │
└────────────────────────────────────────────────────────────┘
```

### Canvas Rendering

Use an HTML `<canvas>` element for the 32x32 grid. Each cell is rendered as a square tile (sized to fit the viewport, e.g., 16px per cell = 512px, or responsive).

**Layers (painted in order):**

1. **Background:** Light tan/brown base color for all squares
2. **Revealed eggs:** Painted in the egg's pastel color at revealed positions
3. **Overlay:** Unrevealed squares get a pixel-art forest tile

**Pixel-Art Overlay:**

The overlay is a 32x32 grid of tiles forming a simple forest/meadow scene. Each unrevealed tile shows a portion of this scene. Tiles are colored using a small palette:

```
Dark green   #2D6A4F  (tree canopy)
Medium green #40916C  (bushes)
Light green  #52B788  (grass)
Pale green   #95D5B2  (meadow)
Brown        #6B4423  (tree trunks)
Yellow       #FFD60A  (flowers, sparse)
Pink         #FF85A1  (flowers, sparse)
```

The overlay pattern is generated deterministically from the game seed so it's consistent across clients. Algorithm:
1. Seed a PRNG (client-side, using the game seed from the API)
2. For each cell, assign a base tile type: mostly grass (60%), some bushes (20%), trees (15%), flowers (5%)
3. Trees span 1x2 cells (trunk below, canopy above) — placed first, then fill remaining cells
4. The result is a simple but recognizable nature scene

When a square is clicked, its overlay tile is removed revealing the background (empty) or egg color underneath.

### Interaction

1. On mount, fetch `GET /games/easter-hunt/state` — render board from response
2. On click:
   a. If not logged in → show login prompt
   b. If on cooldown → show remaining time (no API call, tracked client-side)
   c. Otherwise → `POST /games/easter-hunt/clicks` with `{x, y}`
   d. On success → animate the tile reveal, update board state locally, start cooldown timer
   e. On 429 → sync cooldown timer from response
   f. On 403 (locked) → disable clicks, show "game over" message
   g. On 409 → show "already revealed" (stale state — refetch board)
3. Poll `GET /games/easter-hunt/state` every 10 seconds to see other players' clicks
4. Cooldown countdown displayed in the sidebar, ticking down client-side (synced on each API response)

### Tile Reveal Animation

Simple CSS transition: the overlay tile fades out over 300ms, revealing the color underneath. When another player's click comes in via polling, the reveal happens instantly (no animation).

### Admin Controls

Visible only when `localStorage.getItem('user_role') === 'Admin'`:
- **Reset button:** Confirms with a dialog, then `POST /games/easter-hunt/admin/reset`
- **Reveal toggle:** Fetches `GET /games/easter-hunt/admin/reveal` and overlays egg outlines on the board (dashed borders around egg shapes). Toggle off hides them. This is a client-side visual — doesn't change what other users see.

### Game Over State

When `is_locked` is true (current time >= `lock_at`):
- Board remains visible in its final state
- Clicks are disabled
- Header shows "The hunt is over!"
- Scoreboard shows final standings

---

## File Placement

### Server

```
aspirant-server/
├── server/
│   ├── data_models/
│   │   └── easter_hunt.go          # EasterHuntGame, EasterHuntClick, EasterHuntScore
│   ├── data_functions/
│   │   └── easter_hunt.go          # Egg placement algorithm, board computation
│   ├── handlers/
│   │   └── easter_hunt.go          # All 5 endpoint handlers
│   ├── routes.go                   # Add new route group (edit existing)
│   └── database.go                 # Add AutoMigrate calls (edit existing)
```

### Client

```
aspirant-client/
├── src/
│   ├── views/
│   │   └── EasterHuntView.vue      # Full page component
│   ├── router/
│   │   └── router.js               # Add route (edit existing)
```

One view component is sufficient — the game board, scoreboard, cooldown, and admin controls all live in the same page. No shared components needed beyond what already exists.

---

## Development Plan

### Phase 1 — Server: Data Model and Egg Algorithm
1. Create `server/data_models/easter_hunt.go` with the three GORM models
2. Create `server/data_functions/easter_hunt.go` with the seed-based egg placement algorithm
3. Add AutoMigrate calls in `database.go`
4. Write a unit test for the egg algorithm: determinism (same seed → same output), no overlaps, correct counts

### Phase 2 — Server: API Endpoints
5. Create `server/handlers/easter_hunt.go` with all 5 handlers
6. Register routes in `routes.go` (public, auth, admin groups)
7. Test endpoints manually with curl

### Phase 3 — Client: Game Board
8. Create `EasterHuntView.vue` with canvas rendering, overlay generation, and click handling
9. Add route in `router.js`
10. Test the full flow: reset game (admin), click squares, observe cooldown, view scoreboard

### Phase 4 — Polish
11. Add tile reveal animation
12. Add countdown timer for game lock
13. Test edge cases: cooldown enforcement, locked game, concurrent clicks on same square
14. Add an ApplicationCard entry on the home/games page so users can discover the game
