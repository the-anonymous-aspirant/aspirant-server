package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newStateDB(t *testing.T) (*gorm.DB, Room) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &RelationshipAction{}, &DiceRoll{}, &ConstellationProfile{})
	SeedRelationshipTypes(db)
	room, _, _ := CreateRoom(db, 1, 4)
	JoinRoom(db, 2, room.Code)
	return db, room
}

func TestBuildRoomState_ComposesEverything(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	// Member 1 has a game identity + an avatar; member 2 has neither.
	db.Create(&User{Username: "u1", Email: "u1@x", Password: "p", AvatarETag: "etag1"})
	SetConstellationUsername(db, 1, "Nova")

	types, _ := GetRelationshipTypes(db)
	partner := types[0]
	// A relationship, a dice roll, and thus some history.
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, partner.ID); err != nil {
		t.Fatalf("set: %v", err)
	}
	RollDice(db, room)

	st, err := BuildRoomState(db, room)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}

	if st.Code != room.Code || st.PlayerCount != 4 || st.Occupancy != 2 {
		t.Fatalf("room fields wrong: %+v", st)
	}
	if len(st.Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(st.Members))
	}
	// Member 1 enriched with identity + avatar; member 2 empty-but-present.
	var m1, m2 RoomStateMember
	for _, m := range st.Members {
		if m.UserID == 1 {
			m1 = m
		} else if m.UserID == 2 {
			m2 = m
		}
	}
	if m1.GameUsername != "Nova" || m1.AvatarURL == "" {
		t.Fatalf("member 1 identity/avatar not composed: %+v", m1)
	}
	if m2.GameUsername != "" || m2.AvatarURL != "" {
		t.Fatalf("member 2 should have no identity/avatar: %+v", m2)
	}

	if len(st.Relationships) != 1 {
		t.Fatalf("want 1 relationship, got %d", len(st.Relationships))
	}
	r := st.Relationships[0]
	if r.TypeID != partner.ID || r.Colour != partner.Colour || r.TypeCode != partner.Code {
		t.Fatalf("edge not enriched with type colour: %+v (want colour %s)", r, partner.Colour)
	}

	if st.Dice == nil || st.Dice.Nonce == 0 || len(st.Dice.Faces) == 0 {
		t.Fatalf("dice not composed: %+v", st.Dice)
	}
	if st.HistoryCursor == 0 {
		t.Fatalf("history cursor should be non-zero after an edit")
	}
}

func TestBuildRoomState_EmptyRoom(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	st, err := BuildRoomState(db, room)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}
	if st.Dice != nil {
		t.Fatalf("dice should be nil before any roll, got %+v", st.Dice)
	}
	if len(st.Relationships) != 0 {
		t.Fatalf("no relationships expected, got %d", len(st.Relationships))
	}
	if st.HistoryCursor != 0 {
		t.Fatalf("history cursor should be 0 on an untouched room, got %d", st.HistoryCursor)
	}
}

func TestRoomHistoryCursor_AdvancesOnEdit(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	if c := RoomHistoryCursor(db, room.ID); c != 0 {
		t.Fatalf("empty room cursor want 0, got %d", c)
	}
	types, _ := GetRelationshipTypes(db)
	SetRelationshipWithHistory(db, room, 1, 1, 2, types[0].ID)
	c1 := RoomHistoryCursor(db, room.ID)
	SetRelationshipWithHistory(db, room, 1, 1, 2, types[1].ID)
	c2 := RoomHistoryCursor(db, room.ID)
	if !(c1 > 0 && c2 > c1) {
		t.Fatalf("cursor should advance on each edit: c1=%d c2=%d", c1, c2)
	}
}
