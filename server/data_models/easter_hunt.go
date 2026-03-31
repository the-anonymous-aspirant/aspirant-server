package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

type EasterHuntGame struct {
	gorm.Model
	Seed     int64     `json:"seed" gorm:"not null"`
	LockAt   time.Time `json:"lock_at" gorm:"not null"`
	IsActive bool      `json:"is_active" gorm:"not null;default:true"`
}

type EasterHuntClick struct {
	gorm.Model
	GameID uint `json:"game_id" gorm:"not null;index;uniqueIndex:idx_game_xy"`
	UserID uint `json:"user_id" gorm:"not null;index"`
	X      int  `json:"x" gorm:"not null;uniqueIndex:idx_game_xy"`
	Y      int  `json:"y" gorm:"not null;uniqueIndex:idx_game_xy"`
}

type EasterHuntScore struct {
	gorm.Model
	GameID uint `json:"game_id" gorm:"not null;uniqueIndex:idx_game_user"`
	UserID uint `json:"user_id" gorm:"not null;uniqueIndex:idx_game_user"`
	Score  int  `json:"score" gorm:"not null;default:0"`
}

func GetActiveEasterHuntGame(db *gorm.DB) (*EasterHuntGame, error) {
	var game EasterHuntGame
	err := db.Where("is_active = ?", true).First(&game).Error
	if err != nil {
		return nil, err
	}
	return &game, nil
}

func CreateEasterHuntGame(db *gorm.DB, seed int64, lockAt time.Time) (*EasterHuntGame, error) {
	// Deactivate any existing active games
	db.Model(&EasterHuntGame{}).Where("is_active = ?", true).Update("is_active", false)

	game := EasterHuntGame{
		Seed:     seed,
		LockAt:   lockAt,
		IsActive: true,
	}
	if err := db.Create(&game).Error; err != nil {
		return nil, err
	}
	return &game, nil
}

func GetEasterHuntClicks(db *gorm.DB, gameID uint) ([]EasterHuntClick, error) {
	var clicks []EasterHuntClick
	err := db.Where("game_id = ?", gameID).Find(&clicks).Error
	return clicks, err
}

func GetLastEasterHuntClick(db *gorm.DB, gameID uint, userID uint) (*EasterHuntClick, error) {
	var click EasterHuntClick
	err := db.Where("game_id = ? AND user_id = ?", gameID, userID).
		Order("created_at DESC").First(&click).Error
	if err != nil {
		return nil, err
	}
	return &click, nil
}

func CreateEasterHuntClick(db *gorm.DB, gameID uint, userID uint, x int, y int) (*EasterHuntClick, error) {
	click := EasterHuntClick{
		GameID: gameID,
		UserID: userID,
		X:      x,
		Y:      y,
	}
	if err := db.Create(&click).Error; err != nil {
		return nil, err
	}
	return &click, nil
}

func UpsertEasterHuntScore(db *gorm.DB, gameID uint, userID uint) error {
	var score EasterHuntScore
	err := db.Where("game_id = ? AND user_id = ?", gameID, userID).First(&score).Error
	if err == gorm.ErrRecordNotFound {
		score = EasterHuntScore{GameID: gameID, UserID: userID, Score: 1}
		return db.Create(&score).Error
	}
	if err != nil {
		return err
	}
	return db.Model(&score).Update("score", score.Score+1).Error
}

func GetEasterHuntScores(db *gorm.DB, gameID uint) ([]EasterHuntScore, error) {
	var scores []EasterHuntScore
	err := db.Where("game_id = ?", gameID).Order("score DESC, created_at ASC").Find(&scores).Error
	return scores, err
}

func DeleteEasterHuntGameData(db *gorm.DB, gameID uint) error {
	if err := db.Where("game_id = ?", gameID).Delete(&EasterHuntClick{}).Error; err != nil {
		return err
	}
	if err := db.Where("game_id = ?", gameID).Delete(&EasterHuntScore{}).Error; err != nil {
		return err
	}
	return nil
}
