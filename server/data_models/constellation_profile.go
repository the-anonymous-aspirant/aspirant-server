package data_models

import "github.com/jinzhu/gorm"

// ConstellationProfile is a user's persisted default game identity for the
// Constellations app (epic #4587, subtask #4595-A3): a game username distinct
// from the login credential — User.Username is never mutated. One mutable row
// per user, keyed by UserID. The operator asked for editable defaults, not a
// history table (unlike UserDisplayName), so this is a plain upsert. The user
// icon reuses the existing avatar (User.AvatarETag); no icon column lives here.
// A per-room name override is carried on room membership (subtask A1), not on
// this default.
type ConstellationProfile struct {
	gorm.Model
	UserID       uint   `json:"user_id" gorm:"unique;not null;index"`
	GameUsername string `json:"game_username" gorm:"type:varchar(40);not null"`
}

// TableName pins the physical table name.
func (ConstellationProfile) TableName() string { return "constellation_profiles" }

// GetConstellationProfile returns the user's profile and whether one exists.
// A user without a profile yet is a normal state (they have not set a game
// username), reported as ok=false rather than an error.
func GetConstellationProfile(db *gorm.DB, userID uint) (ConstellationProfile, bool) {
	var p ConstellationProfile
	if err := db.Where("user_id = ?", userID).First(&p).Error; err != nil {
		return ConstellationProfile{}, false
	}
	return p, true
}

// SetConstellationUsername upserts the user's game username on the single row
// keyed by user_id. Creating on first set, updating in place thereafter, so
// there is never more than one row per user. Re-setting the same name is a
// no-op. Returns the resulting profile.
func SetConstellationUsername(db *gorm.DB, userID uint, name string) (ConstellationProfile, error) {
	var p ConstellationProfile
	err := db.Where("user_id = ?", userID).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		p = ConstellationProfile{UserID: userID, GameUsername: name}
		if cerr := db.Create(&p).Error; cerr != nil {
			return ConstellationProfile{}, cerr
		}
		return p, nil
	}
	if err != nil {
		return ConstellationProfile{}, err
	}
	if p.GameUsername != name {
		if uerr := db.Model(&p).Update("game_username", name).Error; uerr != nil {
			return ConstellationProfile{}, uerr
		}
	}
	return p, nil
}
