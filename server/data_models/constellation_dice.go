package data_models

import (
	"crypto/rand"
	"strconv"
	"strings"

	"github.com/jinzhu/gorm"
)

// Constellations server-authoritative dice roll (epic #4587, subtask #4597-B2).
//
// Everyone in a room sees the same dice. A roll is resolved server-side and
// persisted; clients read the resolved value (via D1's snapshot, or the read
// endpoint here) and animate locally toward it over ~2s, so all viewers
// converge on the same faces. The nonce (the roll row's id) is what lets a
// client detect a NEW roll and re-trigger its spin.
//
// Q2 (one die vs two) is operator-resolved to ONE die (epic c25747/c25775);
// DiceCount keeps that a config value so a later change to two dice is a
// one-line edit, not a schema change. Each face is uniform in [1, DiceFaces].

const (
	DiceCount = 1
	DiceFaces = 6
)

// DiceRoll is one resolved roll for a room. The room's current roll is the
// latest row; the nonce is this row's id and rolled_at is its created_at.
type DiceRoll struct {
	gorm.Model
	RoomID uint   `json:"room_id" gorm:"not null;index"`
	Faces  string `json:"-" gorm:"type:varchar(32);not null"` // CSV of face values
}

// TableName pins the physical table name.
func (DiceRoll) TableName() string { return "constellation_dice_rolls" }

// FaceValues parses the stored CSV faces into ints.
func (d DiceRoll) FaceValues() []int {
	if d.Faces == "" {
		return []int{}
	}
	parts := strings.Split(d.Faces, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// rollFace returns a uniform face in [1, DiceFaces] from a crypto source, with
// rejection sampling so no face is biased.
func rollFace() (int, error) {
	// 256 is a multiple of 6? No — reject bytes >= 252 to keep [0,252) which is
	// an exact multiple of DiceFaces, so the modulo is unbiased.
	limit := byte(256 - (256 % DiceFaces))
	buf := make([]byte, 1)
	for {
		if _, err := rand.Read(buf); err != nil {
			return 0, err
		}
		if buf[0] < limit {
			return int(buf[0]%DiceFaces) + 1, nil
		}
	}
}

// RollDice resolves DiceCount faces server-side and persists the roll for the
// room. Returns the created roll (its id is the nonce).
func RollDice(db *gorm.DB, room Room) (DiceRoll, error) {
	faces := make([]string, DiceCount)
	for i := 0; i < DiceCount; i++ {
		f, err := rollFace()
		if err != nil {
			return DiceRoll{}, err
		}
		faces[i] = strconv.Itoa(f)
	}
	roll := DiceRoll{RoomID: room.ID, Faces: strings.Join(faces, ",")}
	if err := db.Create(&roll).Error; err != nil {
		return DiceRoll{}, err
	}
	return roll, nil
}

// CurrentRoll returns the room's latest roll and whether one exists.
func CurrentRoll(db *gorm.DB, roomID uint) (DiceRoll, bool) {
	var roll DiceRoll
	err := db.Where("room_id = ?", roomID).Order("id DESC").First(&roll).Error
	if err != nil {
		return DiceRoll{}, false
	}
	return roll, true
}
