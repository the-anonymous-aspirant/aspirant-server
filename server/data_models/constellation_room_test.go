package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newRoomTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&Room{}, &RoomMember{})
	return db
}

// Create seats the creator in slot 1 with a 5-char code and occupancy 1.
func TestCreateRoomSeatsCreator(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, member, err := CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if len(room.Code) != RoomCodeLength {
		t.Errorf("code %q length = %d, want %d", room.Code, len(room.Code), RoomCodeLength)
	}
	if room.Status != roomStatusActive {
		t.Errorf("status = %q, want active", room.Status)
	}
	if member.Slot != 1 {
		t.Errorf("creator slot = %d, want 1", member.Slot)
	}
	if occ := RoomOccupancy(db, room.ID); occ != 1 {
		t.Errorf("occupancy = %d, want 1", occ)
	}
}

// Player count must be within [MinPlayers, MaxPlayers].
func TestCreateRoomRejectsBadPlayerCount(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	for _, pc := range []int{0, 1, 9, -3} {
		if _, _, err := CreateRoom(db, 1, pc); err != ErrInvalidPlayerCount {
			t.Errorf("CreateRoom(pc=%d) err = %v, want ErrInvalidPlayerCount", pc, err)
		}
	}
}

// A user already in an active game cannot create or join another.
func TestOneGameAtATime(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	first, _, err := CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, _, err := CreateRoom(db, 7, 4); err != ErrAlreadyInGame {
		t.Errorf("second CreateRoom err = %v, want ErrAlreadyInGame", err)
	}

	other, _, err := CreateRoom(db, 99, 4)
	if err != nil {
		t.Fatalf("CreateRoom other: %v", err)
	}
	if _, _, err := JoinRoom(db, 7, other.Code); err != ErrAlreadyInGame {
		t.Errorf("join while in a game err = %v, want ErrAlreadyInGame", err)
	}

	// After leaving, the same user may join elsewhere.
	if _, err := LeaveRoom(db, 7, first.Code); err != nil {
		t.Fatalf("LeaveRoom: %v", err)
	}
	if _, _, err := JoinRoom(db, 7, other.Code); err != nil {
		t.Errorf("join after leaving err = %v, want nil", err)
	}
}

// Joiners are assigned the next free slot in numerically increasing order (Q7).
func TestJoinAssignsIncreasingSlots(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 1, 4) // creator = slot 1
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	_, m2, _ := JoinRoom(db, 2, room.Code)
	_, m3, _ := JoinRoom(db, 3, room.Code)
	if m2.Slot != 2 {
		t.Errorf("user 2 slot = %d, want 2", m2.Slot)
	}
	if m3.Slot != 3 {
		t.Errorf("user 3 slot = %d, want 3", m3.Slot)
	}
	if occ := RoomOccupancy(db, room.ID); occ != 3 {
		t.Errorf("occupancy = %d, want 3", occ)
	}
}

// A slot freed by a leaver is reused by the next joiner (smallest free).
func TestLeaveFreesSlotForReuse(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, _ := CreateRoom(db, 1, 4) // slot 1
	JoinRoom(db, 2, room.Code)         // slot 2
	JoinRoom(db, 3, room.Code)         // slot 3

	if _, err := LeaveRoom(db, 2, room.Code); err != nil { // frees slot 2
		t.Fatalf("LeaveRoom: %v", err)
	}
	_, m4, err := JoinRoom(db, 4, room.Code)
	if err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	if m4.Slot != 2 {
		t.Errorf("reused slot = %d, want 2 (smallest free)", m4.Slot)
	}
}

// A full room refuses new joiners.
func TestJoinFullRoom(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, _ := CreateRoom(db, 1, 2) // capacity 2, creator slot 1
	JoinRoom(db, 2, room.Code)         // slot 2 -> full
	if _, _, err := JoinRoom(db, 3, room.Code); err != ErrRoomFull {
		t.Errorf("join full room err = %v, want ErrRoomFull", err)
	}
}

// Joining a code that never named a room is a plain not-found — distinct from
// the ended-game refusal covered by TestLastLeaveOfPlayedRoomSlates (#4806 ask 1).
func TestJoinUnknownCode(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()
	if _, _, err := JoinRoom(db, 1, "ZZZZZ"); err != ErrRoomNotFound {
		t.Errorf("join unknown code err = %v, want ErrRoomNotFound", err)
	}
	if EndedRoomExists(db, "ZZZZZ") {
		t.Errorf("EndedRoomExists(\"ZZZZZ\") = true, want false for a never-used code")
	}
}

// A room that is still live never reads as ended, so a joinable code cannot be
// mis-explained as a finished game.
func TestEndedRoomExistsFalseForLiveRoom(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()
	room, _, err := CreateRoom(db, 1, 2)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if EndedRoomExists(db, room.Code) {
		t.Errorf("EndedRoomExists(%q) = true for a live room", room.Code)
	}
}

// Re-joining the same room is idempotent — no duplicate membership.
func TestJoinIsIdempotent(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, creator, _ := CreateRoom(db, 1, 4)
	_, again, err := JoinRoom(db, 1, room.Code)
	if err != nil {
		t.Fatalf("re-join: %v", err)
	}
	if again.ID != creator.ID {
		t.Errorf("re-join created a new membership (%d != %d)", again.ID, creator.ID)
	}
	if occ := RoomOccupancy(db, room.ID); occ != 1 {
		t.Errorf("occupancy after re-join = %d, want 1", occ)
	}
}

// When the last member leaves, the room is slated: completed + soft-deleted,
// and no longer joinable.
// A PLAYED room — one that held >=2 members at the same time — slates when its
// last member leaves: status=completed, soft-deleted, code reusable. The
// "played" precondition is the #4785 gate; a never-played solo room is the
// opposite case, covered by TestSoloCreatorLeaveKeepsRoomJoinable.
func TestLastLeaveOfPlayedRoomSlates(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, _ := CreateRoom(db, 1, 4)
	if _, _, err := JoinRoom(db, 2, room.Code); err != nil { // now played: 2 in-room
		t.Fatalf("JoinRoom: %v", err)
	}
	if _, err := LeaveRoom(db, 2, room.Code); err != nil { // occupancy 1, not yet empty
		t.Fatalf("LeaveRoom(2): %v", err)
	}
	if _, ok := GetActiveRoomByCode(db, room.Code); !ok {
		t.Fatalf("room slated while a member still remained")
	}
	if _, err := LeaveRoom(db, 1, room.Code); err != nil { // last member of a played room
		t.Fatalf("LeaveRoom(1): %v", err)
	}

	if _, ok := GetActiveRoomByCode(db, room.Code); ok {
		t.Errorf("slated room is still active-findable")
	}
	var slated Room
	if err := db.Unscoped().Where("id = ?", room.ID).First(&slated).Error; err != nil {
		t.Fatalf("unscoped read: %v", err)
	}
	if slated.Status != roomStatusCompleted {
		t.Errorf("slated status = %q, want completed", slated.Status)
	}
	if slated.DeletedAt == nil {
		t.Errorf("slated room has nil deleted_at; expected soft-delete")
	}
	// #4806 ask 1: a slated room and a never-used code are both "no live room",
	// but the player following a join link needs to be told which — "that game
	// has ended" is a different answer from "no such code".
	if _, _, err := JoinRoom(db, 3, room.Code); err != ErrRoomEnded {
		t.Errorf("join slated room err = %v, want ErrRoomEnded", err)
	}
	if !EndedRoomExists(db, room.Code) {
		t.Errorf("EndedRoomExists(%q) = false, want true for a slated room", room.Code)
	}
}

// #4785 core: a solo creator who leaves a room nobody else ever joined must NOT
// slate it — the shared code stays joinable so a second player can still get in.
// This is the exact operator scenario: create -> leave -> second user joins.
func TestSoloCreatorLeaveKeepsRoomJoinable(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, err := LeaveRoom(db, 1, room.Code); err != nil {
		t.Fatalf("LeaveRoom: %v", err)
	}

	// The room is never-played (peaked at one member), so leaving must not slate it.
	got, ok := GetActiveRoomByCode(db, room.Code)
	if !ok {
		t.Fatalf("solo creator's leave slated a never-played room (the #4785 bug)")
	}
	if occ := RoomOccupancy(db, got.ID); occ != 0 {
		t.Errorf("occupancy = %d, want 0 (creator left, room kept)", occ)
	}
	// A fresh second user can still join the shared code.
	if _, _, err := JoinRoom(db, 2, room.Code); err != nil {
		t.Errorf("second user join after solo-creator leave err = %v, want nil", err)
	}
}

// #4785, logout variant: LeaveAllActiveRooms (the #4778 logout path) must also
// leave a never-played solo room joinable — otherwise logout re-creates the very
// slate-on-empty foot-gun this fix removes.
func TestSoloCreatorLogoutKeepsRoomJoinable(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if err := LeaveAllActiveRooms(db, 7); err != nil {
		t.Fatalf("LeaveAllActiveRooms: %v", err)
	}
	if _, ok := GetActiveRoomByCode(db, room.Code); !ok {
		t.Fatalf("logout slated a never-played solo room (the #4785 bug on the logout path)")
	}
	if _, _, err := JoinRoom(db, 8, room.Code); err != nil {
		t.Errorf("second user join after solo-creator logout err = %v, want nil", err)
	}
}

// Leaving a room the user is not in is refused.
func TestLeaveWhenNotInRoom(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()
	room, _, _ := CreateRoom(db, 1, 4)
	if _, err := LeaveRoom(db, 2, room.Code); err != ErrNotInRoom {
		t.Errorf("leave when not in room err = %v, want ErrNotInRoom", err)
	}
}

// Two live rooms never share a code.
func TestLiveRoomsHaveDistinctCodes(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	r1, _, _ := CreateRoom(db, 1, 4)
	r2, _, _ := CreateRoom(db, 2, 4)
	if r1.Code == r2.Code {
		t.Fatalf("two live rooms share code %q", r1.Code)
	}
}

// codeIsFree encodes the Q6 rule: an active code is not free; an unused code is
// free; a completed (soft-deleted) room's code is free and its stale row +
// members are wiped so the code can be re-inserted under the unique index.
func TestCodeIsFree(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	// Unused code -> free.
	if free, err := codeIsFree(db, "AAAAA"); err != nil || !free {
		t.Fatalf("unused code: free=%v err=%v, want free", free, err)
	}

	// Active room's code -> not free.
	active, _, _ := CreateRoom(db, 1, 4)
	if free, err := codeIsFree(db, active.Code); err != nil || free {
		t.Fatalf("active code %q: free=%v err=%v, want not free", active.Code, free, err)
	}

	// Slated room's code -> free, and the stale row + members are wiped. The
	// room must have been PLAYED (>=2 members at once) to slate on empty now
	// (#4785), so a second user joins before both leave.
	slated, _, _ := CreateRoom(db, 2, 4)
	slatedCode, slatedID := slated.Code, slated.ID
	if _, _, err := JoinRoom(db, 3, slatedCode); err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	if _, err := LeaveRoom(db, 3, slatedCode); err != nil {
		t.Fatalf("LeaveRoom(3): %v", err)
	}
	if _, err := LeaveRoom(db, 2, slatedCode); err != nil { // last member slates
		t.Fatalf("LeaveRoom(2): %v", err)
	}
	free, err := codeIsFree(db, slatedCode)
	if err != nil || !free {
		t.Fatalf("slated code %q: free=%v err=%v, want free", slatedCode, free, err)
	}
	var rooms, members int
	db.Unscoped().Model(&Room{}).Where("id = ?", slatedID).Count(&rooms)
	db.Unscoped().Model(&RoomMember{}).Where("room_id = ?", slatedID).Count(&members)
	if rooms != 0 || members != 0 {
		t.Errorf("stale room not wiped: rooms=%d members=%d, want 0/0", rooms, members)
	}

	// The reclaimed code can now back a fresh live room.
	fresh := Room{Code: slatedCode, PlayerCount: 4, Status: roomStatusActive}
	if err := db.Create(&fresh).Error; err != nil {
		t.Errorf("reusing reclaimed code failed: %v", err)
	}
}

// randomRoomCode only emits characters from the unambiguous alphabet.
func TestRandomRoomCodeAlphabet(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := randomRoomCode()
		if err != nil {
			t.Fatalf("randomRoomCode: %v", err)
		}
		if len(code) != RoomCodeLength {
			t.Fatalf("code %q length = %d", code, len(code))
		}
		for _, ch := range code {
			if !containsRune(roomCodeAlphabet, ch) {
				t.Fatalf("code %q contains %q outside alphabet", code, ch)
			}
		}
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// LeaveAllActiveRooms releases the one-game-at-a-time lock (#4778): after it, a
// user who logged out mid-game (never clicking Leave) can create or join again.
// The abandoned room's fate is the #4785 concern and is asserted separately
// (TestSoloCreatorLogoutKeepsRoomJoinable); here the invariant is the lock.
func TestLeaveAllActiveRoomsReleasesLock(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	if _, _, err := CreateRoom(db, 7, 4); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	// The lock is held: a second create is refused (the logged-out state before
	// the fix — membership outlives the session).
	if _, _, err := CreateRoom(db, 7, 4); err != ErrAlreadyInGame {
		t.Fatalf("pre-leave second CreateRoom err = %v, want ErrAlreadyInGame", err)
	}

	// Logout leaves every active room.
	if err := LeaveAllActiveRooms(db, 7); err != nil {
		t.Fatalf("LeaveAllActiveRooms: %v", err)
	}

	// The membership row is released, so the one-game lock is lifted regardless
	// of whether the room itself slated (a never-played room now stays active,
	// per #4785).
	if _, inGame := activeMembership(db, 7); inGame {
		t.Errorf("user 7 still holds an active membership after logout leave-all")
	}
	// The lock is released: the same user can create again.
	if _, _, err := CreateRoom(db, 7, 4); err != nil {
		t.Errorf("post-leave CreateRoom err = %v, want nil (lock should be released)", err)
	}
}

// LeaveAllActiveRooms on a user in no game is a no-op — logout must be safe
// whether or not a game is open.
func TestLeaveAllActiveRoomsNoActiveGameIsNoop(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	if err := LeaveAllActiveRooms(db, 42); err != nil {
		t.Fatalf("LeaveAllActiveRooms with no game: %v", err)
	}
	// The user is still free to create.
	if _, _, err := CreateRoom(db, 42, 3); err != nil {
		t.Errorf("CreateRoom after no-op leave-all err = %v, want nil", err)
	}
}

// A leaver on logout must not slate a room others are still in — only the
// leaving member's seat frees.
func TestLeaveAllActiveRoomsKeepsRoomForRemainingPlayers(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, _, err := JoinRoom(db, 2, room.Code); err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}

	// User 1 logs out.
	if err := LeaveAllActiveRooms(db, 1); err != nil {
		t.Fatalf("LeaveAllActiveRooms: %v", err)
	}

	// Room stays active for user 2, occupancy drops to 1.
	got, ok := GetActiveRoomByCode(db, room.Code)
	if !ok {
		t.Fatalf("room slated while a player remained")
	}
	if occ := RoomOccupancy(db, got.ID); occ != 1 {
		t.Errorf("occupancy = %d, want 1", occ)
	}
	// User 1 is unlocked; user 2 is still held.
	if _, _, err := CreateRoom(db, 1, 3); err != nil {
		t.Errorf("logged-out user 1 CreateRoom err = %v, want nil", err)
	}
	if _, _, err := CreateRoom(db, 2, 3); err != ErrAlreadyInGame {
		t.Errorf("remaining user 2 CreateRoom err = %v, want ErrAlreadyInGame", err)
	}
}
