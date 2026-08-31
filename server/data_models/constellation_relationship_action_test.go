package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// newHistoryDB seeds a room (creator=1) with members 1,2,3, the relationship
// types, and returns the db, the room, and two valid type ids.
func newHistoryDB(t *testing.T) (*gorm.DB, Room, uint, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&Room{}, &RoomMember{}, &RelationshipType{}, &Relationship{}, &RelationshipAction{})
	SeedRelationshipTypes(db)
	room, _, _ := CreateRoom(db, 1, 4)
	JoinRoom(db, 2, room.Code)
	JoinRoom(db, 3, room.Code)
	types, _ := GetRelationshipTypes(db)
	return db, room, types[0].ID, types[1].ID
}

// activeType returns the pair's active edge type id, or 0 if no active edge.
func activeType(db *gorm.DB, roomID, a, b uint) uint {
	if rel, ok := activePairEdge(db, roomID, a, b); ok {
		return rel.TypeID
	}
	return 0
}

func TestHistory_SetThenUndoRestoresPrior(t *testing.T) {
	db, room, t1, t2 := newHistoryDB(t)
	defer db.Close()

	// First set: no prior edge → undo should leave the pair with no active edge.
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, t1); err != nil {
		t.Fatalf("set1: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != t1 {
		t.Fatalf("after set1 want type %d, got %d", t1, got)
	}
	// Second set: prior edge was t1 → undo should restore t1.
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, t2); err != nil {
		t.Fatalf("set2: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != t2 {
		t.Fatalf("after set2 want type %d, got %d", t2, got)
	}

	if err := UndoRelationship(db, room, 1); err != nil {
		t.Fatalf("undo1: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != t1 {
		t.Fatalf("after undo want prior type %d, got %d", t1, got)
	}
	if err := UndoRelationship(db, room, 1); err != nil {
		t.Fatalf("undo2: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != 0 {
		t.Fatalf("after undo to origin want no active edge, got type %d", got)
	}
	// Nothing left to undo.
	if err := UndoRelationship(db, room, 1); err != ErrNothingToUndo {
		t.Fatalf("want ErrNothingToUndo, got %v", err)
	}
}

func TestHistory_RedoReappliesInOrder(t *testing.T) {
	db, room, t1, t2 := newHistoryDB(t)
	defer db.Close()

	SetRelationshipWithHistory(db, room, 1, 1, 2, t1)
	SetRelationshipWithHistory(db, room, 1, 1, 2, t2)
	UndoRelationship(db, room, 1) // -> t1
	UndoRelationship(db, room, 1) // -> none

	if err := RedoRelationship(db, room, 1); err != nil { // redo the t1 set first
		t.Fatalf("redo1: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != t1 {
		t.Fatalf("after redo1 want %d, got %d", t1, got)
	}
	if err := RedoRelationship(db, room, 1); err != nil { // redo the t2 set
		t.Fatalf("redo2: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != t2 {
		t.Fatalf("after redo2 want %d, got %d", t2, got)
	}
	if err := RedoRelationship(db, room, 1); err != ErrNothingToRedo {
		t.Fatalf("want ErrNothingToRedo, got %v", err)
	}
}

func TestHistory_ClearOfRejectionUndoesLikeAnything(t *testing.T) {
	// Q3 ruling: no game-rule special-casing. A cleared edge undoes back to the
	// prior connection like any other edit.
	db, room, rejection, _ := newHistoryDB(t)
	defer db.Close()

	SetRelationshipWithHistory(db, room, 1, 1, 2, rejection)
	if err := ClearRelationshipWithHistory(db, room, 1, 1, 2); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != 0 {
		t.Fatalf("after clear want no active edge, got %d", got)
	}
	if err := UndoRelationship(db, room, 1); err != nil {
		t.Fatalf("undo clear: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != rejection {
		t.Fatalf("undo of clear must restore the prior connection (%d), got %d", rejection, got)
	}
}

func TestHistory_NewEditTruncatesRedoStack(t *testing.T) {
	db, room, t1, t2 := newHistoryDB(t)
	defer db.Close()

	SetRelationshipWithHistory(db, room, 1, 1, 2, t1)
	SetRelationshipWithHistory(db, room, 1, 1, 2, t2)
	UndoRelationship(db, room, 1) // t1, one action undone
	// A new edit invalidates the redo stack.
	SetRelationshipWithHistory(db, room, 1, 1, 3, t1)
	if err := RedoRelationship(db, room, 1); err != ErrNothingToRedo {
		t.Fatalf("a new edit must clear the redo stack; want ErrNothingToRedo, got %v", err)
	}
}

func TestHistory_CapPerPlayer(t *testing.T) {
	db, room, t1, _ := newHistoryDB(t)
	defer db.Close()

	// Player 1 makes cap+5 edits.
	for i := 0; i < HistoryCapPerPlayer+5; i++ {
		if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, t1); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	hist, _ := PlayerHistory(db, room.ID, 1)
	if len(hist) != HistoryCapPerPlayer {
		t.Fatalf("cap not enforced: want %d retained, got %d", HistoryCapPerPlayer, len(hist))
	}
	// Only the newest cap actions can be undone; the (cap+1)th undo is empty.
	for i := 0; i < HistoryCapPerPlayer; i++ {
		if err := UndoRelationship(db, room, 1); err != nil {
			t.Fatalf("undo %d within cap: %v", i, err)
		}
	}
	if err := UndoRelationship(db, room, 1); err != ErrNothingToUndo {
		t.Fatalf("dropped-oldest actions must be un-undoable; want ErrNothingToUndo, got %v", err)
	}
}

func TestHistory_PerPlayerStacksIndependent(t *testing.T) {
	db, room, t1, t2 := newHistoryDB(t)
	defer db.Close()

	SetRelationshipWithHistory(db, room, 1, 1, 2, t1) // player 1
	SetRelationshipWithHistory(db, room, 2, 1, 2, t2) // player 2 edits same pair
	if got := activeType(db, room.ID, 1, 2); got != t2 {
		t.Fatalf("last-write want %d, got %d", t2, got)
	}
	// Player 2 undoes their own edit → restores player 1's t1.
	if err := UndoRelationship(db, room, 2); err != nil {
		t.Fatalf("p2 undo: %v", err)
	}
	if got := activeType(db, room.ID, 1, 2); got != t1 {
		t.Fatalf("p2 undo should restore p1's %d, got %d", t1, got)
	}
	// Player 2 has nothing more; player 1 still has its own action.
	if err := UndoRelationship(db, room, 2); err != ErrNothingToUndo {
		t.Fatalf("p2 stack should be empty, got %v", err)
	}
	if err := UndoRelationship(db, room, 1); err != nil {
		t.Fatalf("p1 should still have its action: %v", err)
	}
}
