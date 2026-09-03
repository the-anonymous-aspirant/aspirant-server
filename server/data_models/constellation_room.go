package data_models

import (
	"crypto/rand"
	"errors"
	"time"

	"github.com/jinzhu/gorm"
)

// Constellations rooms & membership (epic #4587, subtask #4593-A1).
//
// A Room is one live game — the shared "Connections sheet" a group agrees on.
// A RoomMember is a person's seat in that game. This layer owns the room
// lifecycle only (create / join / leave / occupancy); the relationship graph,
// dice, and live-state snapshot are later subtasks.
//
// Operator rulings that bind this layer (epic #4587 c25747, recorded against
// this subtask in c25775):
//   - Q7 slot identity: a joining player is ASSIGNED the next free slot in
//     numerically increasing order — they do not choose a position.
//   - Q6 purge: there is NO background sweep. New-room code generation draws
//     only from codes not currently occupied+active; a completed (everyone-has-
//     left) room's code becomes selectable again and its row is wiped and
//     recreated on reuse.
//   - Lifecycle: once everyone has left, the game is slated for deletion
//     (status=completed + deleted_at set).
//   - A person may be logged into only one game at a time.

const (
	// RoomCodeLength is the shared room code length (rulebook wireframe: "XBVGR").
	RoomCodeLength = 5
	// MinPlayers / MaxPlayers bound the declared room size. The board draws up
	// to 8 avatars (epic #4587 "up to 8"); a game needs at least two people.
	MinPlayers = 2
	MaxPlayers = 8
	// roomCodeAlphabet excludes visually ambiguous characters (0/O, 1/I) so a
	// player typing a code read off someone else's screen or a QR fallback is
	// less likely to mis-enter it.
	roomCodeAlphabet    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	roomCodeMaxAttempts = 64
	roomStatusActive    = "active"
	roomStatusCompleted = "completed"
)

// Sentinel errors so the handler layer can map to HTTP status codes without
// string-matching.
var (
	ErrRoomNotFound = errors.New("room not found")
	// ErrRoomEnded is a code that DID name a room, whose game has since ended
	// (slated: status=completed + soft-deleted). Both it and ErrRoomNotFound
	// mean "no live room here", but they are different explanations to a player
	// who followed a join link — "that game is over" vs "no such code" — so the
	// join path tells them apart (#4806 ask 1).
	ErrRoomEnded          = errors.New("room has ended")
	ErrRoomFull           = errors.New("room is full")
	ErrAlreadyInGame      = errors.New("user is already in an active game")
	ErrInvalidPlayerCount = errors.New("player count must be between 2 and 8")
	ErrCodeSpaceExhausted = errors.New("could not allocate a free room code")
	ErrNotInRoom          = errors.New("user is not in this room")
	// ErrNotRoomCreator is returned by SetRoomReveal when a non-creator tries to
	// change the room's transparency settings (#4835). "Only the room creator can
	// set" is a server-side authorization rule, not a hidden control.
	ErrNotRoomCreator = errors.New("only the room creator may change room settings")
)

// Room is one live Constellations game. Code is unique among live rows; a
// completed room is soft-deleted (deleted_at) and its code is reusable.
type Room struct {
	gorm.Model
	Code        string `json:"code" gorm:"type:varchar(5);unique;not null;index"`
	PlayerCount int    `json:"player_count" gorm:"not null"`
	Status      string `json:"status" gorm:"type:varchar(16);not null;default:'active'"`
	// EverHadTwoMembers latches once the room has held >=2 members at the same
	// time — i.e. was actually played. The slate-on-empty rule only fires once
	// this is set, so a solo creator who leaves (or logs out, #4778) before
	// anyone joins does NOT slate the room: the shared code stays joinable for
	// the second player instead of 404ing (#4785). A room that never reached two
	// members lingers on empty until a real session forms; reaping those is a
	// separate TTL concern, out of scope here.
	EverHadTwoMembers bool `json:"ever_had_two_members" gorm:"not null;default:false"`
	// CreatorUserID is the user who created the room, seated in slot 1 at
	// creation. It is IMMUTABLE and durable — unlike slot 1, which nextFreeSlot
	// reuses when a leaver frees it, so a room whose creator left while others
	// remained could see a non-creator holding slot 1 (#4835). The creator-only
	// room settings (RevealConnections / RevealCards) authorize against THIS,
	// never against slot ordering. Default 0 so AutoMigrate's ALTER succeeds on
	// pre-#4835 rows; 0 means "no creator recorded", and since no user has id 0
	// the settings are frozen (uneditable by anyone) on such legacy rooms. When
	// the creator leaves, the column does not transfer: the settings freeze at
	// their current value because the authorization matches nobody (#4835).
	CreatorUserID uint `json:"creator_user_id" gorm:"not null;default:0;index"`
	// RevealConnections / RevealCards are the two creator-only transparency
	// toggles (#4835). Default off, preserving the per-viewer privacy shipped by
	// #4809 (edge scoping) and #4807 (goal-card privacy). They relax READ only,
	// in the BuildRoomState serializer; the relationship WRITE path (#4834,
	// endpoint-only) is untouched, so transparency never re-opens cross-party
	// writes. Only the creator may set them (SetRoomReveal).
	RevealConnections bool `json:"reveal_connections" gorm:"not null;default:false"`
	RevealCards       bool `json:"reveal_cards" gorm:"not null;default:false"`
}

// TableName pins the physical table name.
func (Room) TableName() string { return "constellation_rooms" }

// RoomMember is a person's seat in a room. CreatedAt is the joined-at instant;
// LeftAt is nil while the member is currently in-room and set when they leave.
type RoomMember struct {
	gorm.Model
	RoomID uint       `json:"room_id" gorm:"not null;index"`
	UserID uint       `json:"user_id" gorm:"not null;index"`
	Slot   int        `json:"slot" gorm:"not null"`
	LeftAt *time.Time `json:"left_at"`
}

// TableName pins the physical table name.
func (RoomMember) TableName() string { return "constellation_room_members" }

// randomRoomCode returns a RoomCodeLength code drawn from roomCodeAlphabet using
// a cryptographic source, so codes are not predictable from one another.
func randomRoomCode() (string, error) {
	buf := make([]byte, RoomCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, RoomCodeLength)
	n := byte(len(roomCodeAlphabet))
	for i, b := range buf {
		out[i] = roomCodeAlphabet[b%n]
	}
	return string(out), nil
}

// codeIsFree reports whether code may be assigned to a new room. Per the Q6
// ruling: a code held by an active room is not free; a code held only by a
// completed (soft-deleted) room is free, and that stale row and its members are
// wiped here so the unique index stays consistent when the new live row is
// inserted; an unused code is free.
func codeIsFree(db *gorm.DB, code string) (bool, error) {
	var existing Room
	// Unscoped so soft-deleted (completed) rooms are visible — they still hold
	// the code in the unique index and must be considered.
	err := db.Unscoped().Where("code = ?", code).First(&existing).Error
	if gorm.IsRecordNotFoundError(err) {
		return true, nil // unused
	}
	if err != nil {
		return false, err
	}
	if existing.DeletedAt == nil {
		return false, nil // occupied by an active room
	}
	// Completed/slated room: reclaim its code by wiping the old row + members.
	if err := db.Unscoped().Where("room_id = ?", existing.ID).Delete(&RoomMember{}).Error; err != nil {
		return false, err
	}
	if err := db.Unscoped().Delete(&existing).Error; err != nil {
		return false, err
	}
	return true, nil
}

// GenerateRoomCode returns a code free for a new room, drawing random codes
// until one is free (see codeIsFree for the Q6 occupancy/reuse rule).
func GenerateRoomCode(db *gorm.DB) (string, error) {
	for attempt := 0; attempt < roomCodeMaxAttempts; attempt++ {
		code, err := randomRoomCode()
		if err != nil {
			return "", err
		}
		free, err := codeIsFree(db, code)
		if err != nil {
			return "", err
		}
		if free {
			return code, nil
		}
	}
	return "", ErrCodeSpaceExhausted
}

// activeMembership returns the user's current in-room membership across all
// active (non-deleted) rooms, and whether one exists.
func activeMembership(db *gorm.DB, userID uint) (RoomMember, bool) {
	var m RoomMember
	err := db.
		Joins("JOIN constellation_rooms r ON r.id = constellation_room_members.room_id AND r.deleted_at IS NULL").
		Where("constellation_room_members.user_id = ? AND constellation_room_members.left_at IS NULL", userID).
		First(&m).Error
	if err != nil {
		return RoomMember{}, false
	}
	return m, true
}

// ActiveRoomForUser returns the room the user is currently seated in, and
// whether one exists. It is the room half of activeMembership: the handler
// layer needs it to name the room in the ErrAlreadyInGame refusal (#4798), so
// a user told "you are already in a game" can be told WHICH game and navigate
// there to leave it.
func ActiveRoomForUser(db *gorm.DB, userID uint) (Room, bool) {
	m, ok := activeMembership(db, userID)
	if !ok {
		return Room{}, false
	}
	var room Room
	if err := db.Where("id = ?", m.RoomID).First(&room).Error; err != nil {
		return Room{}, false
	}
	return room, true
}

// RoomOccupancy counts members currently in-room (left_at IS NULL).
func RoomOccupancy(db *gorm.DB, roomID uint) int {
	var n int
	db.Model(&RoomMember{}).Where("room_id = ? AND left_at IS NULL", roomID).Count(&n)
	return n
}

// RoomMembers returns the in-room members ordered by slot.
func RoomMembers(db *gorm.DB, roomID uint) ([]RoomMember, error) {
	var members []RoomMember
	err := db.Where("room_id = ? AND left_at IS NULL", roomID).Order("slot ASC").Find(&members).Error
	return members, err
}

// EndedRoomExists reports whether code names a room whose game has ENDED —
// slated (status=completed, soft-deleted) — rather than a code that never named
// a room at all. GetActiveRoomByCode cannot tell the two apart: it filters on
// status=active and gorm's soft-delete scope hides the slated row, so both miss.
//
// The distinction is only readable while the ended game is still the most
// recent holder of the code: codeIsFree wipes a completed row when the code is
// drawn for a new room, after which the code reads as never-used again. That is
// the honest answer at that point — the old game is gone AND the code is free.
func EndedRoomExists(db *gorm.DB, code string) bool {
	var room Room
	// Unscoped so the soft-deleted (slated) row is visible.
	if err := db.Unscoped().Where("code = ?", code).First(&room).Error; err != nil {
		return false
	}
	return room.DeletedAt != nil || room.Status == roomStatusCompleted
}

// GetActiveRoomByCode finds a live (active, non-deleted) room by code.
func GetActiveRoomByCode(db *gorm.DB, code string) (Room, bool) {
	var room Room
	err := db.Where("code = ? AND status = ?", code, roomStatusActive).First(&room).Error
	if err != nil {
		return Room{}, false
	}
	return room, true
}

// nextFreeSlot returns the smallest slot in [1, playerCount] not currently held
// by an in-room member — the numerically-increasing assignment of the Q7
// ruling, which also reuses a slot freed by a leaver.
func nextFreeSlot(db *gorm.DB, roomID uint, playerCount int) (int, bool) {
	var members []RoomMember
	db.Where("room_id = ? AND left_at IS NULL", roomID).Find(&members)
	taken := make(map[int]bool, len(members))
	for _, m := range members {
		taken[m.Slot] = true
	}
	for slot := 1; slot <= playerCount; slot++ {
		if !taken[slot] {
			return slot, true
		}
	}
	return 0, false // room full
}

// CreateRoom validates the player count, enforces one-game-at-a-time, allocates
// a free code, persists the room, and seats the creator in slot 1. Returns the
// room and the creator's membership.
func CreateRoom(db *gorm.DB, userID uint, playerCount int) (Room, RoomMember, error) {
	if playerCount < MinPlayers || playerCount > MaxPlayers {
		return Room{}, RoomMember{}, ErrInvalidPlayerCount
	}
	if _, inGame := activeMembership(db, userID); inGame {
		return Room{}, RoomMember{}, ErrAlreadyInGame
	}
	code, err := GenerateRoomCode(db)
	if err != nil {
		return Room{}, RoomMember{}, err
	}
	room := Room{Code: code, PlayerCount: playerCount, Status: roomStatusActive, CreatorUserID: userID}
	if err := db.Create(&room).Error; err != nil {
		return Room{}, RoomMember{}, err
	}
	member := RoomMember{RoomID: room.ID, UserID: userID, Slot: 1}
	if err := db.Create(&member).Error; err != nil {
		return Room{}, RoomMember{}, err
	}
	return room, member, nil
}

// JoinRoom seats a user in the room named by code. Re-joining the same room is
// idempotent (the existing membership is returned) — the scanned-link auto-join
// (#4806 ask 1) leans on that: a member re-opening their own room link must land
// on the board, not be refused. A user already in a DIFFERENT active game is
// refused; a full room is refused; an unknown code and an ended game are refused
// distinguishably (ErrRoomNotFound vs ErrRoomEnded). The joiner is assigned the
// next free slot.
func JoinRoom(db *gorm.DB, userID uint, code string) (Room, RoomMember, error) {
	room, ok := GetActiveRoomByCode(db, code)
	if !ok {
		if EndedRoomExists(db, code) {
			return Room{}, RoomMember{}, ErrRoomEnded
		}
		return Room{}, RoomMember{}, ErrRoomNotFound
	}
	// Already in some active game?
	if existing, inGame := activeMembership(db, userID); inGame {
		if existing.RoomID == room.ID {
			return room, existing, nil // idempotent re-join of the same room
		}
		return Room{}, RoomMember{}, ErrAlreadyInGame
	}
	slot, ok := nextFreeSlot(db, room.ID, room.PlayerCount)
	if !ok {
		return Room{}, RoomMember{}, ErrRoomFull
	}
	member := RoomMember{RoomID: room.ID, UserID: userID, Slot: slot}
	if err := db.Create(&member).Error; err != nil {
		return Room{}, RoomMember{}, err
	}
	// The room counts as played once two members are in it at the same time;
	// from that point the slate-on-empty rule applies (#4785). Latch it so a
	// later solo state (one of the two leaves) still slates when the last goes.
	if !room.EverHadTwoMembers && RoomOccupancy(db, room.ID) >= 2 {
		if err := db.Model(&room).Update("ever_had_two_members", true).Error; err != nil {
			return Room{}, RoomMember{}, err
		}
		room.EverHadTwoMembers = true
	}
	return room, member, nil
}

// maybeSlate slates a room — status=completed + soft-delete, freeing its code —
// when it has emptied (occupancy 0) AND was ever actually played (>=2 members
// at once, per EverHadTwoMembers). A room that never reached two members is
// left active on empty so its shared code stays joinable (the #4785
// solo-creator fix). The caller passes a room already loaded with its current
// column values (EverHadTwoMembers in particular).
func maybeSlate(db *gorm.DB, room Room) error {
	if !room.EverHadTwoMembers || RoomOccupancy(db, room.ID) != 0 {
		return nil
	}
	if err := db.Model(&room).Update("status", roomStatusCompleted).Error; err != nil {
		return err
	}
	return db.Delete(&room).Error // soft-delete = slate
}

// LeaveRoom marks the user's membership left. When the last in-room member of a
// played room leaves, the room is slated for deletion (see maybeSlate); a
// never-played room is left active so its code stays joinable (#4785).
func LeaveRoom(db *gorm.DB, userID uint, code string) (Room, error) {
	room, ok := GetActiveRoomByCode(db, code)
	if !ok {
		return Room{}, ErrRoomNotFound
	}
	var member RoomMember
	err := db.Where("room_id = ? AND user_id = ? AND left_at IS NULL", room.ID, userID).First(&member).Error
	if gorm.IsRecordNotFoundError(err) {
		return Room{}, ErrNotInRoom
	}
	if err != nil {
		return Room{}, err
	}
	now := time.Now()
	if err := db.Model(&member).Update("left_at", now).Error; err != nil {
		return Room{}, err
	}
	if err := maybeSlate(db, room); err != nil {
		return Room{}, err
	}
	return room, nil
}

// IsRoomCreator reports whether userID created the room. A userID of 0, or a
// room whose CreatorUserID is 0 (a pre-#4835 room with no creator recorded),
// is never the creator — so legacy rooms have their transparency settings
// frozen rather than editable by an arbitrary slot-1 holder.
func IsRoomCreator(room Room, userID uint) bool {
	return userID != 0 && room.CreatorUserID != 0 && room.CreatorUserID == userID
}

// SetRoomReveal updates the room's two creator-only transparency toggles
// (#4835). Only the creator may change them — this is the server-side
// authorization, enforced here in the model so every caller path inherits it,
// mirroring how SetPlayerGoal owns the member-only rule. A nil pointer leaves
// that toggle untouched, so the two toggles are independent: the caller may set
// one, the other, or both. Returns the reloaded room on success and
// ErrNotRoomCreator otherwise.
func SetRoomReveal(db *gorm.DB, room Room, userID uint, revealConnections, revealCards *bool) (Room, error) {
	if !IsRoomCreator(room, userID) {
		return Room{}, ErrNotRoomCreator
	}
	updates := map[string]interface{}{}
	if revealConnections != nil {
		updates["reveal_connections"] = *revealConnections
	}
	if revealCards != nil {
		updates["reveal_cards"] = *revealCards
	}
	if len(updates) > 0 {
		if err := db.Model(&room).Updates(updates).Error; err != nil {
			return Room{}, err
		}
	}
	var reloaded Room
	if err := db.Where("id = ?", room.ID).First(&reloaded).Error; err != nil {
		return Room{}, err
	}
	return reloaded, nil
}

// LeaveAllActiveRooms marks every active membership held by the user left,
// slating any room that empties as a result — the same lifecycle LeaveRoom
// applies per room, but keyed on the user rather than a room code.
//
// It exists because ending the auth session (logout) must also end room
// presence (#4778). Membership is tracked by a RoomMember row with left_at
// NULL, and the create/join predicate refuses a user who holds one
// (ErrAlreadyInGame). Logout used to clear only the cookie, so the row lingered
// and the one-game-at-a-time lock stranded the user out of create AND join on
// every future login until the room slated from another player's side. Calling
// this on logout releases the lock the moment the user leaves.
//
// The one-game-at-a-time invariant means a user holds at most one active
// membership; the loop tolerates more than one defensively so a data drift
// cannot leave a residual lock. Idempotent — a user in no active room is a
// no-op — so it is safe on every logout, including a logout with no game open.
func LeaveAllActiveRooms(db *gorm.DB, userID uint) error {
	var members []RoomMember
	if err := db.
		Joins("JOIN constellation_rooms r ON r.id = constellation_room_members.room_id AND r.deleted_at IS NULL").
		Where("constellation_room_members.user_id = ? AND constellation_room_members.left_at IS NULL", userID).
		Find(&members).Error; err != nil {
		return err
	}
	now := time.Now()
	for i := range members {
		m := members[i]
		if err := db.Model(&m).Update("left_at", now).Error; err != nil {
			return err
		}
		var room Room
		if err := db.First(&room, m.RoomID).Error; err != nil {
			// Room already gone; releasing the membership is enough.
			continue
		}
		if err := maybeSlate(db, room); err != nil {
			return err
		}
	}
	return nil
}
