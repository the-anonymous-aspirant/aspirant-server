package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// Scratchpad is a single free-form text buffer per user. One row per user
// (UserID is the natural primary key), overwritten in place on save. The
// buffer is server-side plaintext — no encryption — and is scoped to the
// authenticated session's user, never to a caller-supplied id.
type Scratchpad struct {
	UserID    uint      `json:"-" gorm:"primary_key"`
	Content   string    `json:"text" gorm:"type:text"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"-"`
}

func (Scratchpad) TableName() string { return "scratchpads" }

// GetScratchpad returns the scratchpad row for userID, or (nil, nil) when the
// user has never written one (a first visit is an empty scratchpad, not an error).
func GetScratchpad(db *gorm.DB, userID uint) (*Scratchpad, error) {
	var sp Scratchpad
	err := db.Where("user_id = ?", userID).First(&sp).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// UpsertScratchpad overwrites (or creates) the user's scratchpad with content
// and returns the saved row. GORM v1 (jinzhu fork) has no ON CONFLICT clause,
// so this is a read-then-create-or-save, mirroring UpsertPushupEntry.
func UpsertScratchpad(db *gorm.DB, userID uint, content string) (*Scratchpad, error) {
	existing, err := GetScratchpad(db, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing == nil {
		sp := Scratchpad{
			UserID:    userID,
			Content:   content,
			UpdatedAt: now,
			CreatedAt: now,
		}
		if err := db.Create(&sp).Error; err != nil {
			return nil, err
		}
		return &sp, nil
	}
	existing.Content = content
	existing.UpdatedAt = now
	if err := db.Save(existing).Error; err != nil {
		return nil, err
	}
	return existing, nil
}
