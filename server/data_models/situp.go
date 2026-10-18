package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// SitupEntry holds one day of the Pappas sit-ups challenge
// (2026-10-18 .. 2026-12-13, system_3 #7992). Date is the natural primary
// key — one row per calendar day. Like PushupEntry it carries no user id:
// the log is robert's alone, and that is enforced by the route gate in
// routes.go (#4203's single-tenant decision), not by the schema.
type SitupEntry struct {
	Date      time.Time  `json:"date" gorm:"type:date;primary_key"`
	Count     *int       `json:"count" gorm:"type:integer"`
	UpdatedAt time.Time  `json:"updated_at"`
	UpdatedBy uint       `json:"updated_by"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty" sql:"index"`
}

func GetSitupEntries(db *gorm.DB) ([]SitupEntry, error) {
	var entries []SitupEntry
	err := db.Order("date asc").Find(&entries).Error
	return entries, err
}

func GetSitupEntryByDate(db *gorm.DB, date time.Time) (*SitupEntry, error) {
	var entry SitupEntry
	err := db.Where("date = ?", date).First(&entry).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func UpsertSitupEntry(db *gorm.DB, date time.Time, count *int, userID uint) (*SitupEntry, error) {
	existing, err := GetSitupEntryByDate(db, date)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing == nil {
		entry := SitupEntry{
			Date:      date,
			Count:     count,
			UpdatedAt: now,
			UpdatedBy: userID,
			CreatedAt: now,
		}
		if err := db.Create(&entry).Error; err != nil {
			return nil, err
		}
		return &entry, nil
	}
	existing.Count = count
	existing.UpdatedAt = now
	existing.UpdatedBy = userID
	if err := db.Save(existing).Error; err != nil {
		return nil, err
	}
	return existing, nil
}
