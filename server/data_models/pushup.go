package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// PushupEntry holds one day of the 60-day pushup challenge
// (2026-07-01 .. 2026-08-29). Date is the natural primary key
// — one row per calendar day, regardless of submitter.
type PushupEntry struct {
	Date      time.Time  `json:"date" gorm:"type:date;primary_key"`
	Count     *int       `json:"count" gorm:"type:integer"`
	UpdatedAt time.Time  `json:"updated_at"`
	UpdatedBy uint       `json:"updated_by"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty" sql:"index"`
}

// PushupMilestone is the seeded cumulative-count → Swedish-message map.
// The challenge page joins the running cumulative against this table
// when an entry is saved; on a hit the message_sv string is surfaced.
type PushupMilestone struct {
	CumulativeCount int    `json:"cumulative_count" gorm:"primary_key"`
	MessageSv       string `json:"message_sv" gorm:"not null"`
}

func GetPushupEntries(db *gorm.DB) ([]PushupEntry, error) {
	var entries []PushupEntry
	err := db.Order("date asc").Find(&entries).Error
	return entries, err
}

func GetPushupEntryByDate(db *gorm.DB, date time.Time) (*PushupEntry, error) {
	var entry PushupEntry
	err := db.Where("date = ?", date).First(&entry).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func UpsertPushupEntry(db *gorm.DB, date time.Time, count *int, userID uint) (*PushupEntry, error) {
	existing, err := GetPushupEntryByDate(db, date)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing == nil {
		entry := PushupEntry{
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

func GetPushupMilestones(db *gorm.DB) ([]PushupMilestone, error) {
	var milestones []PushupMilestone
	err := db.Order("cumulative_count asc").Find(&milestones).Error
	return milestones, err
}

// pushupSeedMilestones is the canonical Swedish-message seed list.
// Keep it small, encouraging, and themed around the 1000-target arc.
// Engineer-added entries beyond the operator's examples are marked with
// a short rationale in the trailing comment.
var pushupSeedMilestones = []PushupMilestone{
	{CumulativeCount: 59, MessageSv: "Lika många armhävningar som din ålder — nice!"},
	{CumulativeCount: 100, MessageSv: "100 nere, 900 kvar!"},
	{CumulativeCount: 150, MessageSv: "150 — du är igång på riktigt nu."},
	{CumulativeCount: 200, MessageSv: "200 nere, 800 kvar!"},
	{CumulativeCount: 250, MessageSv: "250 — en fjärdedel klar!"},
	{CumulativeCount: 300, MessageSv: "300 nere, 700 kvar — bra tempo!"},
	{CumulativeCount: 333, MessageSv: "333 — en tredjedel hemma."},
	{CumulativeCount: 400, MessageSv: "400 nere, 600 kvar!"},
	{CumulativeCount: 500, MessageSv: "500 — halvvägs!"},
	{CumulativeCount: 555, MessageSv: "555 — ditt favoritnummer!"},
	{CumulativeCount: 600, MessageSv: "600 nere, 400 kvar — nedförsbacke nu."},
	{CumulativeCount: 666, MessageSv: "666 — två tredjedelar klart!"},
	{CumulativeCount: 700, MessageSv: "700 nere, 300 kvar!"},
	{CumulativeCount: 750, MessageSv: "750 — tre fjärdedelar!"},
	{CumulativeCount: 800, MessageSv: "800 nere, 200 kvar — slutspurt!"},
	{CumulativeCount: 900, MessageSv: "900 nere, 100 kvar — sista hundran!"},
	{CumulativeCount: 999, MessageSv: "999 — en enda till!"},
	{CumulativeCount: 1000, MessageSv: "Mål! Du klarade det!"},
}

// SeedPushupMilestones upserts the canonical list. Safe to call on every
// startup — existing rows keep their message_sv unless we re-add one with
// the same key (then it is overwritten with the in-code copy, which is
// the desired source-of-truth direction).
func SeedPushupMilestones(db *gorm.DB) {
	for _, m := range pushupSeedMilestones {
		var existing PushupMilestone
		err := db.Where("cumulative_count = ?", m.CumulativeCount).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			db.Create(&m)
			continue
		}
		if existing.MessageSv != m.MessageSv {
			db.Model(&existing).Update("message_sv", m.MessageSv)
		}
	}
}
