package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// newRelTestDB migrates the room + type + relationship tables, seeds the six
// relationship types, and returns a room with three in-room members (u1/u2/u3)
// plus a valid type id.
func newRelTestDB(t *testing.T) (*gorm.DB, Room, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&Room{}, &RoomMember{}, &RelationshipType{}, &Relationship{})
	SeedRelationshipTypes(db)

	room, _, err := CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, _, err := JoinRoom(db, 2, room.Code); err != nil {
		t.Fatalf("JoinRoom u2: %v", err)
	}
	if _, _, err := JoinRoom(db, 3, room.Code); err != nil {
		t.Fatalf("JoinRoom u3: %v", err)
	}
	types, _ := GetRelationshipTypes(db)
	if len(types) == 0 {
		t.Fatalf("no relationship types seeded")
	}
	return db, room, types[0].ID
}

func TestSetRelationshipCreates(t *testing.T) {
	db, room, typeID := newRelTestDB(t)
	defer db.Close()

	rel, err := SetRelationship(db, room, 1, 2, typeID)
	if err != nil {
		t.Fatalf("SetRelationship: %v", err)
	}
	if rel.FromUserID != 1 || rel.ToUserID != 2 || rel.TypeID != typeID {
		t.Errorf("edge = %+v, want from=1 to=2 type=%d", rel, typeID)
	}
	rels, _ := RoomRelationships(db, room.ID)
	if len(rels) != 1 {
		t.Errorf("active edges = %d, want 1", len(rels))
	}
}

// Setting the same pair in the opposite order updates the one edge, not a
// second — the symmetry constraint — and the latest direction wins.
func TestSetRelationshipSymmetry(t *testing.T) {
	db, room, typeID := newRelTestDB(t)
	defer db.Close()

	SetRelationship(db, room, 1, 2, typeID)
	types, _ := GetRelationshipTypes(db)
	otherType := types[1].ID
	rel, err := SetRelationship(db, room, 2, 1, otherType) // opposite order, new type
	if err != nil {
		t.Fatalf("SetRelationship reverse: %v", err)
	}

	rels, _ := RoomRelationships(db, room.ID)
	if len(rels) != 1 {
		t.Fatalf("active edges = %d, want 1 (one edge per pair)", len(rels))
	}
	if rel.FromUserID != 2 || rel.ToUserID != 1 {
		t.Errorf("direction not updated to latest: %+v", rel)
	}
	if rel.TypeID != otherType {
		t.Errorf("type = %d, want %d (latest set wins)", rel.TypeID, otherType)
	}
}

func TestSetRelationshipValidation(t *testing.T) {
	db, room, typeID := newRelTestDB(t)
	defer db.Close()

	if _, err := SetRelationship(db, room, 1, 1, typeID); err != ErrRelSameMember {
		t.Errorf("same member err = %v, want ErrRelSameMember", err)
	}
	if _, err := SetRelationship(db, room, 1, 99, typeID); err != ErrRelNotMember {
		t.Errorf("non-member err = %v, want ErrRelNotMember", err)
	}
	if _, err := SetRelationship(db, room, 1, 2, 99999); err != ErrRelInvalidType {
		t.Errorf("bad type err = %v, want ErrRelInvalidType", err)
	}
}

func TestClearRelationship(t *testing.T) {
	db, room, typeID := newRelTestDB(t)
	defer db.Close()

	SetRelationship(db, room, 1, 2, typeID)
	if err := ClearRelationship(db, room, 2, 1); err != nil { // opposite order clears the pair
		t.Fatalf("ClearRelationship: %v", err)
	}
	rels, _ := RoomRelationships(db, room.ID)
	if len(rels) != 0 {
		t.Errorf("active edges after clear = %d, want 0", len(rels))
	}
	// Clearing again → no active edge.
	if err := ClearRelationship(db, room, 1, 2); err != ErrRelNoActiveEdge {
		t.Errorf("double clear err = %v, want ErrRelNoActiveEdge", err)
	}
	// The cleared row is retained (for C1 history).
	var total int
	db.Model(&Relationship{}).Where("room_id = ?", room.ID).Count(&total)
	if total != 1 {
		t.Errorf("cleared row not retained: total=%d, want 1", total)
	}
}

// A fresh set after a clear creates a new active edge.
func TestSetAfterClear(t *testing.T) {
	db, room, typeID := newRelTestDB(t)
	defer db.Close()

	SetRelationship(db, room, 1, 2, typeID)
	ClearRelationship(db, room, 1, 2)
	if _, err := SetRelationship(db, room, 1, 2, typeID); err != nil {
		t.Fatalf("set after clear: %v", err)
	}
	rels, _ := RoomRelationships(db, room.ID)
	if len(rels) != 1 {
		t.Errorf("active edges = %d, want 1", len(rels))
	}
}

// Edges are scoped to their room.
func TestRelationshipsScopedToRoom(t *testing.T) {
	db, room, typeID := newRelTestDB(t)
	defer db.Close()

	SetRelationship(db, room, 1, 2, typeID)

	// A second room with its own members and edge.
	other, _, _ := CreateRoom(db, 10, 4)
	JoinRoom(db, 11, other.Code)
	SetRelationship(db, other, 10, 11, typeID)

	a, _ := RoomRelationships(db, room.ID)
	b, _ := RoomRelationships(db, other.ID)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("room edges leaked: room=%d other=%d, want 1/1", len(a), len(b))
	}
	if a[0].RoomID != room.ID || b[0].RoomID != other.ID {
		t.Errorf("edge room scoping wrong: %d / %d", a[0].RoomID, b[0].RoomID)
	}
}
