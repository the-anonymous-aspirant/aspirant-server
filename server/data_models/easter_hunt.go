package data_models

import (
	"fmt"
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

// EasterHuntEgg stores one egg's metadata for a game.
type EasterHuntEgg struct {
	gorm.Model
	GameID            uint       `json:"game_id" gorm:"not null;index;uniqueIndex:idx_game_egg"`
	EggIndex          int        `json:"egg_index" gorm:"not null;uniqueIndex:idx_game_egg"` // 0-23
	Color             string     `json:"color" gorm:"not null"`
	TotalCells        int        `json:"total_cells" gorm:"not null"`
	CompletedByUserID *uint      `json:"completed_by_user_id"`
	CompletedAt       *time.Time `json:"completed_at"`
}

// EasterHuntEggCell stores one cell of an egg's shape.
type EasterHuntEggCell struct {
	gorm.Model
	GameID   uint `json:"game_id" gorm:"not null;index:idx_egg_cell_game"`
	EggIndex int  `json:"egg_index" gorm:"not null;index:idx_egg_cell_game"`
	X        int  `json:"x" gorm:"not null"`
	Y        int  `json:"y" gorm:"not null"`
}

// ── Game ──

func GetActiveEasterHuntGame(db *gorm.DB) (*EasterHuntGame, error) {
	var game EasterHuntGame
	err := db.Where("is_active = ?", true).First(&game).Error
	if err != nil {
		return nil, err
	}
	return &game, nil
}

func CreateEasterHuntGame(db *gorm.DB, seed int64, lockAt time.Time) (*EasterHuntGame, error) {
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

// ── Clicks ──

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
	// Check if already revealed
	var existing EasterHuntClick
	if err := db.Where("game_id = ? AND x = ? AND y = ?", gameID, x, y).First(&existing).Error; err == nil {
		return nil, fmt.Errorf("already revealed")
	}
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

// ── Scores ──

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

// ── Eggs ──

func CreateEasterHuntEgg(db *gorm.DB, egg *EasterHuntEgg) error {
	return db.Create(egg).Error
}

func CreateEasterHuntEggCells(db *gorm.DB, cells []EasterHuntEggCell) error {
	for i := range cells {
		if err := db.Create(&cells[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

func GetEasterHuntEggs(db *gorm.DB, gameID uint) ([]EasterHuntEgg, error) {
	var eggs []EasterHuntEgg
	err := db.Where("game_id = ?", gameID).Order("egg_index ASC").Find(&eggs).Error
	return eggs, err
}

func GetEasterHuntEggCells(db *gorm.DB, gameID uint) ([]EasterHuntEggCell, error) {
	var cells []EasterHuntEggCell
	err := db.Where("game_id = ?", gameID).Find(&cells).Error
	return cells, err
}

// CountRevealedCellsForEgg counts how many of an egg's cells have been revealed.
func CountRevealedCellsForEgg(db *gorm.DB, gameID uint, eggIndex int) int {
	var count int
	db.Raw(`
		SELECT COUNT(*) FROM easter_hunt_clicks c
		INNER JOIN easter_hunt_egg_cells ec
		ON c.game_id = ec.game_id AND c.x = ec.x AND c.y = ec.y
		WHERE c.game_id = ? AND ec.egg_index = ? AND c.deleted_at IS NULL AND ec.deleted_at IS NULL
	`, gameID, eggIndex).Row().Scan(&count)
	return count
}

// MarkEggCompleted sets the completer and timestamp on an egg. Returns false if already completed.
func MarkEggCompleted(db *gorm.DB, gameID uint, eggIndex int, userID uint) bool {
	result := db.Model(&EasterHuntEgg{}).
		Where("game_id = ? AND egg_index = ? AND completed_by_user_id IS NULL", gameID, eggIndex).
		Updates(map[string]interface{}{
			"completed_by_user_id": userID,
			"completed_at":         time.Now().UTC(),
		})
	return result.RowsAffected > 0
}

// ── Cleanup ──

func DeleteEasterHuntGameData(db *gorm.DB, gameID uint) error {
	if err := db.Where("game_id = ?", gameID).Delete(&EasterHuntClick{}).Error; err != nil {
		return err
	}
	if err := db.Where("game_id = ?", gameID).Delete(&EasterHuntScore{}).Error; err != nil {
		return err
	}
	if err := db.Where("game_id = ?", gameID).Delete(&EasterHuntEggCell{}).Error; err != nil {
		return err
	}
	if err := db.Where("game_id = ?", gameID).Delete(&EasterHuntEgg{}).Error; err != nil {
		return err
	}
	return nil
}
