package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newDiceTestDB(t *testing.T) (*gorm.DB, Room) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&Room{}, &RoomMember{}, &DiceRoll{})
	room, _, err := CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	return db, room
}

func TestRollDiceProducesFaces(t *testing.T) {
	db, room := newDiceTestDB(t)
	defer db.Close()

	roll, err := RollDice(db, room)
	if err != nil {
		t.Fatalf("RollDice: %v", err)
	}
	faces := roll.FaceValues()
	if len(faces) != DiceCount {
		t.Fatalf("got %d faces, want %d", len(faces), DiceCount)
	}
	for _, f := range faces {
		if f < 1 || f > DiceFaces {
			t.Errorf("face %d out of range [1,%d]", f, DiceFaces)
		}
	}
}

// Rolling many times only ever yields in-range faces (exercises rejection
// sampling across the whole space).
func TestRollFaceRange(t *testing.T) {
	db, room := newDiceTestDB(t)
	defer db.Close()
	seen := map[int]bool{}
	for i := 0; i < 300; i++ {
		roll, err := RollDice(db, room)
		if err != nil {
			t.Fatalf("RollDice: %v", err)
		}
		for _, f := range roll.FaceValues() {
			if f < 1 || f > DiceFaces {
				t.Fatalf("face %d out of range", f)
			}
			seen[f] = true
		}
	}
	if len(seen) != DiceFaces {
		t.Errorf("only saw faces %v across 300 rolls; expected all %d", seen, DiceFaces)
	}
}

// Acceptance: after a roll, two successive reads return the same faces + nonce.
func TestCurrentRollStable(t *testing.T) {
	db, room := newDiceTestDB(t)
	defer db.Close()

	rolled, _ := RollDice(db, room)
	a, ok1 := CurrentRoll(db, room.ID)
	b, ok2 := CurrentRoll(db, room.ID)
	if !ok1 || !ok2 {
		t.Fatalf("CurrentRoll not found after a roll")
	}
	if a.ID != rolled.ID || b.ID != rolled.ID {
		t.Errorf("nonce drift: rolled=%d a=%d b=%d", rolled.ID, a.ID, b.ID)
	}
	if a.Faces != b.Faces || a.Faces != rolled.Faces {
		t.Errorf("faces drift across reads: %q %q %q", rolled.Faces, a.Faces, b.Faces)
	}
}

// A new roll advances the nonce (so clients detect it).
func TestRollAdvancesNonce(t *testing.T) {
	db, room := newDiceTestDB(t)
	defer db.Close()

	first, _ := RollDice(db, room)
	second, _ := RollDice(db, room)
	if second.ID <= first.ID {
		t.Errorf("nonce did not advance: first=%d second=%d", first.ID, second.ID)
	}
	cur, _ := CurrentRoll(db, room.ID)
	if cur.ID != second.ID {
		t.Errorf("current roll = %d, want latest %d", cur.ID, second.ID)
	}
}

// A room that has never rolled reports no current roll.
func TestNeverRolled(t *testing.T) {
	db, room := newDiceTestDB(t)
	defer db.Close()
	if _, ok := CurrentRoll(db, room.ID); ok {
		t.Errorf("expected no current roll for a never-rolled room")
	}
}
