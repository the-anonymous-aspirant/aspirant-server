package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #4835 — creator persistence + the two creator-only transparency toggles
// (reveal others' connections, reveal others' relationship cards). Both
// toggles are read relaxations in BuildRoomState; the endpoint-only write path
// (#4834) is left closed. Tests reuse the helpers in the sibling _test files
// (newRoomTestDB, newStateDB, newHistoryDB, activeType) — same package.

func boolPtr(b bool) *bool { return &b }

// CreateRoom records the creator durably and defaults both toggles off.
func TestCreateRoomRecordsCreator(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if room.CreatorUserID != 7 {
		t.Errorf("CreatorUserID = %d, want 7", room.CreatorUserID)
	}
	var reloaded Room
	if err := db.First(&reloaded, room.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.CreatorUserID != 7 {
		t.Errorf("persisted CreatorUserID = %d, want 7", reloaded.CreatorUserID)
	}
	if reloaded.RevealConnections || reloaded.RevealCards {
		t.Errorf("toggles should default off, got %+v", reloaded)
	}
}

// Only the creator may set the toggles, and the two are independent.
func TestSetRoomReveal_CreatorOnlyAndIndependent(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 7, 4) // creator = 7
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// A non-creator (even a member) cannot change settings.
	if _, err := SetRoomReveal(db, room, 8, boolPtr(true), nil); err != ErrNotRoomCreator {
		t.Fatalf("non-creator set want ErrNotRoomCreator, got %v", err)
	}

	// Creator sets connections only — cards untouched (independent).
	updated, err := SetRoomReveal(db, room, 7, boolPtr(true), nil)
	if err != nil {
		t.Fatalf("creator set connections: %v", err)
	}
	if !updated.RevealConnections || updated.RevealCards {
		t.Fatalf("want connections on, cards off, got %+v", updated)
	}

	// Then cards only — connections stays on.
	updated, err = SetRoomReveal(db, updated, 7, nil, boolPtr(true))
	if err != nil {
		t.Fatalf("creator set cards: %v", err)
	}
	if !updated.RevealConnections || !updated.RevealCards {
		t.Fatalf("want both on, got %+v", updated)
	}

	// Turn connections back off independently — cards stays on.
	updated, err = SetRoomReveal(db, updated, 7, boolPtr(false), nil)
	if err != nil {
		t.Fatalf("creator clear connections: %v", err)
	}
	if updated.RevealConnections || !updated.RevealCards {
		t.Fatalf("want connections off, cards on, got %+v", updated)
	}
}

// A pre-#4835 room (no creator recorded) has its settings frozen for everyone —
// this is also the "creator leaves" outcome, since CreatorUserID never transfers.
func TestSetRoomReveal_LegacyRoomFrozen(t *testing.T) {
	db := newRoomTestDB(t)
	defer db.Close()

	room, _, err := CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	db.Model(&room).Update("creator_user_id", 0)
	room.CreatorUserID = 0

	if _, err := SetRoomReveal(db, room, 7, boolPtr(true), nil); err != ErrNotRoomCreator {
		t.Fatalf("legacy room must be frozen for everyone, got %v", err)
	}
}

// RevealConnections relaxes the per-viewer edge filter: off, a viewer sees only
// edges they are an endpoint of; on, they see every edge in the room.
func TestBuildRoomState_RevealConnections(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	if _, _, err := JoinRoom(db, 3, room.Code); err != nil {
		t.Fatalf("join u3: %v", err)
	}
	types, _ := GetRelationshipTypes(db)
	partner := types[0]
	// Edge between 2 and 3 — viewer 1 is NOT an endpoint.
	if _, err := SetRelationshipWithHistory(db, room, 2, 2, 3, partner.ID); err != nil {
		t.Fatalf("set 2-3: %v", err)
	}

	// Off (default): viewer 1 sees none of it.
	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("build off: %v", err)
	}
	if len(st.Relationships) != 0 {
		t.Fatalf("reveal off: viewer 1 should see 0 edges, got %d", len(st.Relationships))
	}
	if st.RevealConnections {
		t.Fatalf("RevealConnections should default false")
	}

	// On: viewer 1 now sees the 2-3 edge.
	db.Model(&room).Update("reveal_connections", true)
	room.RevealConnections = true
	st, err = BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("build on: %v", err)
	}
	if len(st.Relationships) != 1 {
		t.Fatalf("reveal on: viewer 1 should see the 2-3 edge, got %d", len(st.Relationships))
	}
	if !st.RevealConnections {
		t.Fatalf("RevealConnections should echo true")
	}
}

// IsCreator is a per-viewer flag: true for the room's creator, false otherwise.
func TestBuildRoomState_IsCreator(t *testing.T) {
	db, room := newStateDB(t) // creator = user 1, member 2 joined
	defer db.Close()

	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("build creator: %v", err)
	}
	if !st.IsCreator {
		t.Fatalf("creator viewer should have IsCreator true")
	}
	st, err = BuildRoomState(db, room, 2)
	if err != nil {
		t.Fatalf("build non-creator: %v", err)
	}
	if st.IsCreator {
		t.Fatalf("non-creator viewer should have IsCreator false")
	}
}

// RevealCards exposes every member's goal card. Off, another member's card is
// nil (the #4807 default) while the viewer's own still surfaces in RoomState.Goal.
func TestBuildRoomState_RevealCards(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{}, &Relationship{},
		&RelationshipAction{}, &RelationshipEvent{}, &DiceRoll{}, &ConstellationProfile{},
		&GoalCard{}, &PlayerGoal{})
	SeedRelationshipTypes(db)
	SeedGoalCards(db)

	room, _, err := CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, _, err := JoinRoom(db, 2, room.Code); err != nil {
		t.Fatalf("join u2: %v", err)
	}
	cards, _ := GetGoalCards(db)
	if len(cards) < 2 {
		t.Fatalf("need >=2 goal cards, got %d", len(cards))
	}
	if _, err := SetPlayerGoal(db, room, 1, cards[0].ID); err != nil {
		t.Fatalf("goal u1: %v", err)
	}
	if _, err := SetPlayerGoal(db, room, 2, cards[1].ID); err != nil {
		t.Fatalf("goal u2: %v", err)
	}

	// Off: viewer 1's own goal surfaces top-level; no member carries a card.
	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("build off: %v", err)
	}
	if st.Goal == nil || st.Goal.CardID != cards[0].ID {
		t.Fatalf("viewer's own goal should surface top-level, got %+v", st.Goal)
	}
	for _, m := range st.Members {
		if m.Goal != nil {
			t.Fatalf("reveal off: member %d goal should be nil, got %+v", m.UserID, m.Goal)
		}
	}
	if st.RevealCards {
		t.Fatalf("RevealCards should default false")
	}

	// On: every member's card is exposed.
	db.Model(&room).Update("reveal_cards", true)
	room.RevealCards = true
	st, err = BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("build on: %v", err)
	}
	got := map[uint]uint{}
	for _, m := range st.Members {
		if m.Goal != nil {
			got[m.UserID] = m.Goal.CardID
		}
	}
	if got[1] != cards[0].ID || got[2] != cards[1].ID {
		t.Fatalf("reveal on: want {1:%d,2:%d}, got %v", cards[0].ID, cards[1].ID, got)
	}
	if !st.RevealCards {
		t.Fatalf("RevealCards should echo true")
	}
}

// The #4835 guard: the transparency toggles relax READ only. With both on, a
// non-endpoint write is STILL refused — transparency does not re-open #4834.
func TestWriteAuthz_RevealTogglesDoNotOpenWritePath(t *testing.T) {
	db, room, t1, _ := newHistoryDB(t) // room creator 1, members 1/2/3
	defer db.Close()

	if err := db.Model(&room).Updates(map[string]interface{}{
		"reveal_connections": true,
		"reveal_cards":       true,
	}).Error; err != nil {
		t.Fatalf("enable reveal toggles: %v", err)
	}
	room.RevealConnections = true
	room.RevealCards = true

	// Player 3 now SEES every edge, but still may not write the 1-2 edge.
	if _, err := SetRelationshipWithHistory(db, room, 3, 1, 2, t1); err != ErrRelNotEndpoint {
		t.Fatalf("set by non-endpoint with reveal on want ErrRelNotEndpoint, got %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != 0 {
		t.Fatalf("refused set must not create the edge, got type %d", got)
	}

	// An endpoint establishes it; a non-endpoint clear is still refused.
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, t1); err != nil {
		t.Fatalf("endpoint set should succeed, got %v", err)
	}
	if err := ClearRelationshipWithHistory(db, room, 3, 1, 2); err != ErrRelNotEndpoint {
		t.Fatalf("clear by non-endpoint with reveal on want ErrRelNotEndpoint, got %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got == 0 {
		t.Fatal("refused clear must not remove the edge")
	}
}
