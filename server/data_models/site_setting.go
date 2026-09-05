package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// SiteSetting holds a site-wide policy flag: one row per setting, keyed by
// name (system_3 epic #5113, subtask #5289).
//
// A keyed table rather than a column on a settings struct, because the next
// flag should not be a schema change and gorm's AutoMigrate adding columns to
// a one-row table is a worse shape than inserting a row.
//
// It is deliberately NOT a general-purpose configuration store. Everything the
// server reads from the environment stays in the environment; what belongs here
// is the small set of switches an admin flips at runtime, from the admin page,
// while the process keeps running.
type SiteSetting struct {
	Key       string `gorm:"primary_key"`
	Value     string
	UpdatedAt time.Time
}

// TableName pins the table name rather than relying on the pluraliser, since
// this is a migration surface.
func (SiteSetting) TableName() string { return "site_settings" }

// SettingSignupEnabled is the kill-switch the operator asked for: with it off,
// the site accepts no new self-service accounts.
const SettingSignupEnabled = "signup_enabled"

// settingTrue / settingFalse are the stored spellings. Constants so a writer
// and a reader in different files cannot disagree about the encoding.
const (
	settingTrue  = "true"
	settingFalse = "false"
)

// BoolSetting reads a boolean flag, returning defaultValue when no row exists.
//
// The absent row IS the default, and that is what makes this safe to deploy:
// no seed statement has to run before the first request, and a fresh database
// behaves like a configured one. Only an explicit write ever departs from the
// default.
//
// A read error is returned rather than swallowed into the default. A flag whose
// failure mode is "quietly behave as though nobody set it" is not a flag: for
// SettingSignupEnabled specifically, swallowing would mean a database blip
// reopens public sign-up on a site the operator closed. Callers decide, and the
// two callers here both refuse the request.
func BoolSetting(db *gorm.DB, key string, defaultValue bool) (bool, error) {
	var setting SiteSetting
	err := db.Where("key = ?", key).First(&setting).Error
	if gorm.IsRecordNotFoundError(err) {
		return defaultValue, nil
	}
	if err != nil {
		return false, err
	}
	return setting.Value == settingTrue, nil
}

// SetBoolSetting writes a boolean flag, creating the row on first write.
//
// Save-or-create rather than gorm's Save alone: the row's primary key is the
// caller-supplied string, so gorm cannot tell a new row from an existing one by
// looking at a zero id, and Save on a missing row is an UPDATE that matches
// nothing and reports success.
func SetBoolSetting(db *gorm.DB, key string, enabled bool) error {
	value := settingFalse
	if enabled {
		value = settingTrue
	}

	var existing SiteSetting
	err := db.Where("key = ?", key).First(&existing).Error
	if gorm.IsRecordNotFoundError(err) {
		return db.Create(&SiteSetting{Key: key, Value: value, UpdatedAt: time.Now()}).Error
	}
	if err != nil {
		return err
	}
	return db.Model(&SiteSetting{}).Where("key = ?", key).
		Updates(map[string]interface{}{"value": value, "updated_at": time.Now()}).Error
}

// SignupEnabled reports whether public self-service sign-up is open.
//
// Default true: the site has had open sign-up since #5220 and a deploy of this
// change must not silently close it.
func SignupEnabled(db *gorm.DB) (bool, error) {
	return BoolSetting(db, SettingSignupEnabled, true)
}

// SetSignupEnabled opens or closes public self-service sign-up site-wide.
func SetSignupEnabled(db *gorm.DB, enabled bool) error {
	return SetBoolSetting(db, SettingSignupEnabled, enabled)
}
