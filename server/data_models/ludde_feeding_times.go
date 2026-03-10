package data_models

import (
	"github.com/jinzhu/gorm"
)

type LuddeFeedingTime struct {
	gorm.Model
	Timestamp string `json:"timestamp" gorm:"not null"`
	Comment   string `json:"comment"`
}

// CreateFeedingTime creates a new feeding time
func (l *LuddeFeedingTime) CreateFeedingTime(db *gorm.DB) error {
	return db.Create(l).Error
}

// DeleteFeedingTime deletes a feeding time from the database
func (l *LuddeFeedingTime) DeleteFeedingTime(db *gorm.DB) error {
	return db.Delete(l).Error
}

// GetAllFeedingTimes retrieves all feeding times from the database
func GetAllFeedingTimes(db *gorm.DB) ([]LuddeFeedingTime, error) {
	var feedingTimes []LuddeFeedingTime
	err := db.Find(&feedingTimes).Error
	if err != nil {
		return nil, err
	}
	return feedingTimes, nil
}
