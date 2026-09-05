package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newPlayerGoalTestDB(t *testing.T) (*gorm.DB, Room) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &GoalCard{}, &PlayerGoal{})
	// Production creates this in database.go right after AutoMigrate; without
	// it here the fixtures would test a table shape production does not have,
	// which is how the missing constraint went unnoticed in the first place.
	if err := EnsurePlayerGoalUniqueIndex(db); err != nil {
		t.Fatalf("ensure unique index: %v", err)
	}
	SeedGoalCards(db)
	room, _, _ := CreateRoom(db, 1, 4)
	JoinRoom(db, 2, room.Code)
	return db, room
}

func firstTwoCards(t *testing.T, db *gorm.DB) (GoalCard, GoalCard) {
	t.Helper()
	cards, err := GetGoalCards(db)
	if err != nil || len(cards) < 2 {
		t.Fatalf("need >=2 goal cards, got %d (%v)", len(cards), err)
	}
	return cards[0], cards[1]
}

func TestSetPlayerGoal_SetGetClear(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, _ := firstTwoCards(t, db)

	if _, err := SetPlayerGoal(db, room, 1, c1.ID); err != nil {
		t.Fatalf("SetPlayerGoal: %v", err)
	}
	got, ok := GetPlayerGoal(db, room.ID, 1)
	if !ok || got.ID != c1.ID {
		t.Fatalf("GetPlayerGoal after set = (%+v, %v), want card %d", got, ok, c1.ID)
	}

	if err := ClearPlayerGoal(db, room, 1); err != nil {
		t.Fatalf("ClearPlayerGoal: %v", err)
	}
	if _, ok := GetPlayerGoal(db, room.ID, 1); ok {
		t.Fatalf("goal still present after clear")
	}
	// Clearing again is not an error (idempotent).
	if err := ClearPlayerGoal(db, room, 1); err != nil {
		t.Fatalf("second ClearPlayerGoal should be a no-op, got %v", err)
	}
}

func TestSetPlayerGoal_ReplacesOnReselect(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, c2 := firstTwoCards(t, db)

	SetPlayerGoal(db, room, 1, c1.ID)
	SetPlayerGoal(db, room, 1, c2.ID)

	got, ok := GetPlayerGoal(db, room.ID, 1)
	if !ok || got.ID != c2.ID {
		t.Fatalf("reselect did not replace: got %+v, want card %d", got, c2.ID)
	}
	// One goal per player per room, never two rows.
	var n int
	db.Model(&PlayerGoal{}).Where("room_id = ? AND user_id = ?", room.ID, 1).Count(&n)
	if n != 1 {
		t.Fatalf("want exactly 1 PlayerGoal row after reselect, got %d", n)
	}
}

func TestSetPlayerGoal_UnknownCardRejected(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()

	if _, err := SetPlayerGoal(db, room, 1, 99999); err != ErrGoalUnknownCard {
		t.Fatalf("SetPlayerGoal(unknown card) err = %v, want ErrGoalUnknownCard", err)
	}
}

func TestSetPlayerGoal_NonMemberRejected(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, _ := firstTwoCards(t, db)

	// User 3 never joined the room.
	if _, err := SetPlayerGoal(db, room, 3, c1.ID); err != ErrGoalNotMember {
		t.Fatalf("SetPlayerGoal(non-member) err = %v, want ErrGoalNotMember", err)
	}
}

func TestPlayerGoals_AreIndependentPerPlayer(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, c2 := firstTwoCards(t, db)

	SetPlayerGoal(db, room, 1, c1.ID)
	SetPlayerGoal(db, room, 2, c2.ID)

	g1, _ := GetPlayerGoal(db, room.ID, 1)
	g2, _ := GetPlayerGoal(db, room.ID, 2)
	if g1.ID != c1.ID || g2.ID != c2.ID {
		t.Fatalf("goals crossed: p1=%d (want %d), p2=%d (want %d)", g1.ID, c1.ID, g2.ID, c2.ID)
	}
}

// The constraint the struct tag was supposed to carry and never did (#5157).
// Verified load-bearing: with EnsurePlayerGoalUniqueIndex removed from the
// fixture this test fails, so it is asserting the index and not the app path.
func TestPlayerGoalUniqueIndex_RejectsASecondLiveRowForOnePlayer(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, c2 := firstTwoCards(t, db)

	if _, err := SetPlayerGoal(db, room, 1, c1.ID); err != nil {
		t.Fatalf("SetPlayerGoal: %v", err)
	}
	// SetPlayerGoal reads-then-writes, so a concurrent caller can miss the
	// existing row and insert a second. Reproduce that outcome directly: the
	// database must now refuse it.
	dup := PlayerGoal{RoomID: room.ID, UserID: 1, GoalCardID: c2.ID}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("a second live goal row for the same (room, user) was accepted; " +
			"the partial unique index is missing")
	}

	var live int
	db.Model(&PlayerGoal{}).Where("room_id = ? AND user_id = ?", room.ID, 1).Count(&live)
	if live != 1 {
		t.Fatalf("live rows for (room, user) = %d, want 1", live)
	}
}

// The reason the index is PARTIAL. A plain UNIQUE(room_id, user_id) would pass
// the test above and break this one, because ClearPlayerGoal soft-deletes and
// the cleared row keeps occupying the pair.
func TestPlayerGoalUniqueIndex_AllowsReselectionAfterAClear(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, c2 := firstTwoCards(t, db)

	if _, err := SetPlayerGoal(db, room, 1, c1.ID); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := ClearPlayerGoal(db, room, 1); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := SetPlayerGoal(db, room, 1, c2.ID); err != nil {
		t.Fatalf("re-select after clear: %v", err)
	}

	card, ok := GetPlayerGoal(db, room.ID, 1)
	if !ok || card.ID != c2.ID {
		t.Fatalf("GetPlayerGoal after re-select = (%v, %v), want card %d", card.ID, ok, c2.ID)
	}

	// The soft-deleted row is still there — that is what a plain unique index
	// would have collided with.
	var all int
	db.Unscoped().Model(&PlayerGoal{}).Where("room_id = ? AND user_id = ?", room.ID, 1).Count(&all)
	if all < 2 {
		t.Fatalf("rows including soft-deleted = %d, want >= 2 (clear must soft-delete)", all)
	}
}

// Sequential re-selection replaces rather than inserts — the path the app
// actually takes, which the new index must not break.
func TestSetPlayerGoal_ReselectionLeavesOneLiveRow(t *testing.T) {
	db, room := newPlayerGoalTestDB(t)
	defer db.Close()
	c1, c2 := firstTwoCards(t, db)

	if _, err := SetPlayerGoal(db, room, 1, c1.ID); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if _, err := SetPlayerGoal(db, room, 1, c2.ID); err != nil {
		t.Fatalf("second set: %v", err)
	}

	var live int
	db.Model(&PlayerGoal{}).Where("room_id = ? AND user_id = ?", room.ID, 1).Count(&live)
	if live != 1 {
		t.Fatalf("live rows after re-selection = %d, want 1", live)
	}
	if card, ok := GetPlayerGoal(db, room.ID, 1); !ok || card.ID != c2.ID {
		t.Fatalf("selected card = %d (ok=%v), want %d", card.ID, ok, c2.ID)
	}
}
