package data_models

import (
	"strings"
	"time"
	"unicode"
)

// The Christmas cards (system_3 #12582): an admin-only picture puzzle of
// ChristmasCardCount hand-drawn cards. Each card shows a picture; the right
// word under it unlocks the next card, and after the last one a closing
// sentence is shown.
//
// The words are the answers of a real treasure hunt and this repository is
// public, so nothing here holds a word, a picture or the sentence: they live
// only in these rows, written through the Control endpoints.
//
// The picture's bytes are in the row, not in LocalStorage: the store's MD5
// index is what the Member-tier GET /fetch-object/:etag resolves, and a
// picture must reach nobody before the word before it is found.

// ChristmasCardCount is the number of cards. Fixed: the operator drew seven.
const ChristmasCardCount = 7

// MaxChristmasWordRunes bounds a word, as typed on Play and stored on Control
// (the operator's "up to 20 characters").
const MaxChristmasWordRunes = 20

// MaxChristmasSentenceRunes bounds the closing sentence.
const MaxChristmasSentenceRunes = 300

// MaxChristmasPictureBytes bounds one stored picture.
const MaxChristmasPictureBytes = 8 << 20

// ChristmasCard is one card. A card with no word or no picture is not ready,
// and Play stops in front of it.
type ChristmasCard struct {
	Number    int    `gorm:"primary_key;auto_increment:false"`
	Word      string `gorm:"type:varchar(80);not null;default:''"`
	Picture   []byte `gorm:"type:bytea"`
	MediaType string `gorm:"type:varchar(40);not null;default:''"`
	Width     int    `gorm:"not null;default:0"`
	Height    int    `gorm:"not null;default:0"`
	// OpensAt is when Play may show the card and check a word against it
	// (system_3 #12780): one card a day up to Christmas, set on Control. Nil
	// is open now. The server's clock decides, never the browser's.
	OpensAt   *time.Time
	UpdatedAt time.Time
}

func (ChristmasCard) TableName() string { return "christmas_cards" }

// Ready reports whether Play can show the card and check a word against it.
func (c ChristmasCard) Ready() bool {
	return len(c.Picture) > 0 && strings.TrimSpace(c.Word) != ""
}

// OpenAt reports whether the card has opened by now.
func (c ChristmasCard) OpenAt(now time.Time) bool {
	return c.OpensAt == nil || !now.Before(*c.OpensAt)
}

// ChristmasSolve records that a user found a card's word. Progress is per
// user, so opening Play to another player later needs no schema change.
type ChristmasSolve struct {
	ID       uint      `gorm:"primary_key"`
	UserID   uint      `gorm:"not null;unique_index:idx_christmas_solves_user_number"`
	Number   int       `gorm:"not null;unique_index:idx_christmas_solves_user_number"`
	SolvedAt time.Time `gorm:"not null"`
}

func (ChristmasSolve) TableName() string { return "christmas_solves" }

// ChristmasSetting holds the closing sentence: one row, key "sentence".
type ChristmasSetting struct {
	Key       string `gorm:"primary_key;type:varchar(40)"`
	Value     string `gorm:"type:text;not null;default:''"`
	UpdatedAt time.Time
}

func (ChristmasSetting) TableName() string { return "christmas_settings" }

// ChristmasSentenceKey is the closing sentence's row.
const ChristmasSentenceKey = "sentence"

// NormalizeChristmasWord is the one matching rule, applied to the stored word
// and the typed one alike: trim, fold every run of white space to one space,
// lower case. "  Blue   Kite " matches "blue kite"; "bluekite" does not.
func NormalizeChristmasWord(s string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " "))
}
