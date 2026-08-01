package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// UserDisplayName is the temporal record of a user's public display name.
//
// A user's *current* display name is the single row with ValidTo == nil.
// Renaming closes that open row (sets ValidTo = now) and inserts a new open
// row, so the full history is preserved and reversible. The login credential
// (User.Username) is never mutated — it stays a credential-only field that no
// longer crosses a public boundary.
//
// Introduced for security-finding #3094: public leaderboards were printing the
// login username, handing anonymous callers a valid username list.
type UserDisplayName struct {
	gorm.Model
	UserID      uint       `json:"user_id" gorm:"not null;index"`
	DisplayName string     `json:"display_name" gorm:"not null"`
	ValidFrom   time.Time  `json:"valid_from" gorm:"not null"`
	ValidTo     *time.Time `json:"valid_to"`
}

// TableName pins the physical table name.
func (UserDisplayName) TableName() string { return "user_display_names" }

// CurrentDisplayName returns the user's active display name (the open temporal
// row). It falls back to the login username only when no open row exists — a
// state the AutoMigrate backfill (BackfillDisplayNames) and the User
// AfterCreate hook make unreachable in normal operation; the fallback exists so
// a leaderboard row never renders blank, not as a routine path.
func CurrentDisplayName(db *gorm.DB, userID uint) string {
	var udn UserDisplayName
	if err := db.Where("user_id = ? AND valid_to IS NULL", userID).First(&udn).Error; err == nil {
		return udn.DisplayName
	}
	var user User
	if err := db.Where("id = ?", userID).First(&user).Error; err == nil {
		return user.Username
	}
	return ""
}

// CurrentDisplayNames batch-resolves current display names for many users in a
// single query, avoiding an N+1 on the leaderboards. Any userID without an open
// row is filled from the login-username fallback described on
// CurrentDisplayName. IDs that resolve to no user at all are absent from the
// returned map (callers skip such orphan rows, matching prior behaviour).
func CurrentDisplayNames(db *gorm.DB, userIDs []uint) map[uint]string {
	out := make(map[uint]string, len(userIDs))
	if len(userIDs) == 0 {
		return out
	}
	var rows []UserDisplayName
	db.Where("user_id IN (?) AND valid_to IS NULL", userIDs).Find(&rows)
	for _, r := range rows {
		out[r.UserID] = r.DisplayName
	}

	var missing []uint
	for _, id := range userIDs {
		if _, ok := out[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		var users []User
		db.Where("id IN (?)", missing).Find(&users)
		for _, u := range users {
			out[u.ID] = u.Username
		}
	}
	return out
}

// SetDisplayName switches a user's display name: it closes the current open row
// (ValidTo = now) and inserts a new open row, preserving history. If the new
// name equals the current one it is a no-op. When the user has no open row yet
// (e.g. legacy row not backfilled), it simply opens one.
func SetDisplayName(db *gorm.DB, userID uint, name string) error {
	now := time.Now()
	var current UserDisplayName
	err := db.Where("user_id = ? AND valid_to IS NULL", userID).First(&current).Error
	if err == nil {
		if current.DisplayName == name {
			return nil
		}
		if uerr := db.Model(&current).Update("valid_to", now).Error; uerr != nil {
			return uerr
		}
	}
	return db.Create(&UserDisplayName{
		UserID:      userID,
		DisplayName: name,
		ValidFrom:   now,
	}).Error
}

// BackfillDisplayNames opens a display-name row (display_name = username) for
// every user that lacks one, so existing names are visually unchanged on
// deploy. It is idempotent: a user that already has an open row is skipped, so
// re-running AutoMigrate never duplicates. Portable across dialects (used from
// AutoMigrate against Postgres and from tests against SQLite).
func BackfillDisplayNames(db *gorm.DB) error {
	var users []User
	if err := db.Find(&users).Error; err != nil {
		return err
	}
	for _, u := range users {
		var count int
		db.Model(&UserDisplayName{}).Where("user_id = ? AND valid_to IS NULL", u.ID).Count(&count)
		if count > 0 {
			continue
		}
		if err := db.Create(&UserDisplayName{
			UserID:      u.ID,
			DisplayName: u.Username,
			ValidFrom:   time.Now(),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}
