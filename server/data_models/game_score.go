package data_models

import (
	"encoding/json"

	"github.com/jinzhu/gorm"
)

type GameScore struct {
	gorm.Model
	UserID   int             `json:"user_id" gorm:"not null;index"`
	Game     string          `json:"game" gorm:"type:varchar(50);not null;index"`
	Mode     string          `json:"mode" gorm:"type:varchar(30)"`
	Score    int             `json:"score" gorm:"not null"`
	Metadata json.RawMessage `json:"metadata" gorm:"type:jsonb"`
}

func (s *GameScore) PostGameScore(db *gorm.DB) error {
	return db.Create(s).Error
}

func GetGameScores(db *gorm.DB, game string, mode string, limit int) ([]GameScore, error) {
	var scores []GameScore
	query := db.Where("game = ?", game)
	if mode != "" {
		query = query.Where("mode = ?", mode)
	}
	err := query.Order("score DESC").Limit(limit).Find(&scores).Error
	if err != nil {
		return nil, err
	}
	return scores, nil
}
