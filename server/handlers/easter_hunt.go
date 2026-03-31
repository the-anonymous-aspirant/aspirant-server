package handlers

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"

	"aspirant-online/server/data_functions"
	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

const (
	easterHuntCooldown     = 5 * time.Minute
	easterHuntDefaultLock  = "2026-04-05T16:00:00Z" // 18:00 CEST = 16:00 UTC
)

// ---------- Public ----------

func GetEasterHuntStateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	board := data_functions.GenerateEggBoard(game.Seed)
	clicks, err := data_models.GetEasterHuntClicks(db, game.ID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving clicks")
		return
	}

	// Build username lookup
	usernames := loadUsernames(db, clicks)

	// Build revealed squares list
	revealed := make([]gin.H, 0, len(clicks))
	for _, click := range clicks {
		eggID := board.Grid[click.X][click.Y]
		revealed = append(revealed, gin.H{
			"x":        click.X,
			"y":        click.Y,
			"egg_id":   eggID,
			"user_id":  click.UserID,
			"username": usernames[click.UserID],
		})
	}

	// Build egg progress — only eggs with at least 1 revealed square
	revealedPerEgg := countRevealed(clicks, board)
	eggs := buildEggProgress(board, revealedPerEgg, db, game.ID)

	// Scores
	scores := loadScoreboard(db, game.ID)

	now := time.Now().UTC()
	RespondWithSuccess(c, gin.H{
		"game_id":   game.ID,
		"seed":      game.Seed,
		"lock_at":   game.LockAt.UTC().Format(time.RFC3339),
		"is_locked": now.After(game.LockAt) || now.Equal(game.LockAt),
		"board": gin.H{
			"width":    data_functions.BoardWidth,
			"height":   data_functions.BoardHeight,
			"revealed": revealed,
			"eggs":     eggs,
		},
		"scores": scores,
	}, "Game state retrieved")
}

func GetEasterHuntScoresHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	page, pageSize := parsePagination(c)
	offset := (page - 1) * pageSize

	var total int64
	db.Model(&data_models.EasterHuntScore{}).Where("game_id = ?", game.ID).Count(&total)

	var scores []data_models.EasterHuntScore
	db.Where("game_id = ?", game.ID).
		Order("score DESC, created_at ASC").
		Offset(offset).Limit(pageSize).
		Find(&scores)

	items := make([]gin.H, 0, len(scores))
	for _, s := range scores {
		var user data_models.User
		if err := db.Where("id = ?", s.UserID).First(&user).Error; err != nil {
			continue
		}
		items = append(items, gin.H{
			"user_id":  s.UserID,
			"username": user.Username,
			"score":    s.Score,
		})
	}

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// ---------- Authenticated ----------

func PostEasterHuntClickHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID := c.MustGet("user_id").(uint)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	// Check game lock
	now := time.Now().UTC()
	if now.After(game.LockAt) || now.Equal(game.LockAt) {
		respondError(c, http.StatusForbidden, "game_locked", "The hunt has ended")
		return
	}

	// Check cooldown
	lastClick, err := data_models.GetLastEasterHuntClick(db, game.ID, userID)
	if err == nil {
		cooldownUntil := lastClick.CreatedAt.Add(easterHuntCooldown)
		if now.Before(cooldownUntil) {
			remaining := cooldownUntil.Sub(now)
			mins := int(remaining.Minutes())
			secs := int(remaining.Seconds()) % 60
			respondError(c, http.StatusTooManyRequests, "cooldown",
				fmt.Sprintf("You can click again in %dm %ds", mins, secs))
			return
		}
	}

	// Parse body
	var body struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	if body.X < 0 || body.X >= data_functions.BoardWidth || body.Y < 0 || body.Y >= data_functions.BoardHeight {
		RespondWithError(c, http.StatusBadRequest, "x and y must be between 0 and 31")
		return
	}

	// Attempt to create click (unique constraint prevents duplicates)
	click, err := data_models.CreateEasterHuntClick(db, game.ID, userID, body.X, body.Y)
	if err != nil {
		respondError(c, http.StatusConflict, "conflict", "Square already revealed")
		return
	}

	// Check what's under this square
	board := data_functions.GenerateEggBoard(game.Seed)
	eggID := board.Grid[body.X][body.Y]

	eggCompleted := false
	scored := false

	if eggID >= 0 {
		// Count revealed squares for this egg
		clicks, _ := data_models.GetEasterHuntClicks(db, game.ID)
		count := 0
		for _, cl := range clicks {
			if board.Grid[cl.X][cl.Y] == eggID {
				count++
			}
		}
		if count == data_functions.EggSize {
			eggCompleted = true
			scored = true
			if err := data_models.UpsertEasterHuntScore(db, game.ID, userID); err != nil {
				log.Printf("Error upserting score: %v", err)
			}
		}
	}

	cooldownUntil := click.CreatedAt.Add(easterHuntCooldown)
	RespondWithSuccess(c, gin.H{
		"x":                  body.X,
		"y":                  body.Y,
		"egg_id":             eggID,
		"egg_completed":      eggCompleted,
		"scored":             scored,
		"cooldown_until":     cooldownUntil.UTC().Format(time.RFC3339),
		"next_click_seconds": int(easterHuntCooldown.Seconds()),
	}, "Square revealed")
}

func GetEasterHuntCooldownHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID := c.MustGet("user_id").(uint)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	now := time.Now().UTC()
	onCooldown := false
	var cooldownUntil time.Time
	var remaining float64

	lastClick, err := data_models.GetLastEasterHuntClick(db, game.ID, userID)
	if err == nil {
		cooldownUntil = lastClick.CreatedAt.Add(easterHuntCooldown)
		if now.Before(cooldownUntil) {
			onCooldown = true
			remaining = cooldownUntil.Sub(now).Seconds()
		}
	}

	RespondWithSuccess(c, gin.H{
		"on_cooldown":       onCooldown,
		"cooldown_until":    cooldownUntil.UTC().Format(time.RFC3339),
		"remaining_seconds": int(remaining),
	}, "Cooldown status retrieved")
}

// ---------- Admin ----------

func PostEasterHuntResetHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var body struct {
		LockAt string `json:"lock_at"`
	}
	c.ShouldBindJSON(&body)

	lockAt, err := time.Parse(time.RFC3339, easterHuntDefaultLock)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Invalid default lock time")
		return
	}
	if body.LockAt != "" {
		parsed, err := time.Parse(time.RFC3339, body.LockAt)
		if err != nil {
			RespondWithError(c, http.StatusBadRequest, "Invalid lock_at format, use RFC3339")
			return
		}
		lockAt = parsed
	}

	// Clean up old game data
	oldGame, err := data_models.GetActiveEasterHuntGame(db)
	if err == nil {
		data_functions.ClearBoardCache(oldGame.Seed)
		data_models.DeleteEasterHuntGameData(db, oldGame.ID)
	}

	seed := time.Now().UnixNano()
	// Use crypto-quality randomness for the seed isn't needed — time is fine
	seed = int64(rand.New(rand.NewSource(seed)).Int63())

	game, err := data_models.CreateEasterHuntGame(db, seed, lockAt)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Error creating game")
		return
	}

	board := data_functions.GenerateEggBoard(game.Seed)

	RespondWithSuccess(c, gin.H{
		"game_id":   game.ID,
		"seed":      game.Seed,
		"lock_at":   game.LockAt.UTC().Format(time.RFC3339),
		"egg_count": len(board.Eggs),
	}, "Game reset")
}

func GetEasterHuntRevealHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	board := data_functions.GenerateEggBoard(game.Seed)

	eggs := make([]gin.H, 0, len(board.Eggs))
	for _, egg := range board.Eggs {
		squares := make([]gin.H, 0, len(egg.Squares))
		for _, sq := range egg.Squares {
			squares = append(squares, gin.H{"x": sq.X, "y": sq.Y})
		}
		eggs = append(eggs, gin.H{
			"egg_id":  egg.ID,
			"color":   egg.Color,
			"squares": squares,
		})
	}

	RespondWithSuccess(c, gin.H{
		"game_id": game.ID,
		"seed":    game.Seed,
		"eggs":    eggs,
	}, "Full board revealed")
}

// ---------- Helpers ----------

func loadUsernames(db *gorm.DB, clicks []data_models.EasterHuntClick) map[uint]string {
	userIDs := make(map[uint]bool)
	for _, click := range clicks {
		userIDs[click.UserID] = true
	}

	usernames := make(map[uint]string)
	for id := range userIDs {
		var user data_models.User
		if err := db.Where("id = ?", id).First(&user).Error; err == nil {
			usernames[id] = user.Username
		}
	}
	return usernames
}

func countRevealed(clicks []data_models.EasterHuntClick, board *data_functions.EggBoard) map[int]int {
	counts := make(map[int]int)
	for _, click := range clicks {
		eggID := board.Grid[click.X][click.Y]
		if eggID >= 0 {
			counts[eggID]++
		}
	}
	return counts
}

func buildEggProgress(board *data_functions.EggBoard, revealedPerEgg map[int]int, db *gorm.DB, gameID uint) []gin.H {
	eggs := make([]gin.H, 0)
	for _, egg := range board.Eggs {
		revealed, hasRevealed := revealedPerEgg[egg.ID]
		if !hasRevealed {
			continue
		}
		completed := revealed == data_functions.EggSize
		var completedBy interface{} = nil
		if completed {
			completedBy = findEggCompleter(db, gameID, board, egg.ID)
		}
		eggs = append(eggs, gin.H{
			"egg_id":       egg.ID,
			"color":        egg.Color,
			"squares":      data_functions.EggSize,
			"revealed":     revealed,
			"completed":    completed,
			"completed_by": completedBy,
		})
	}
	return eggs
}

func findEggCompleter(db *gorm.DB, gameID uint, board *data_functions.EggBoard, eggID int) string {
	// Find the last click that revealed a square of this egg
	var clicks []data_models.EasterHuntClick
	db.Where("game_id = ?", gameID).Order("created_at ASC").Find(&clicks)

	var lastClickUserID uint
	count := 0
	for _, click := range clicks {
		if board.Grid[click.X][click.Y] == eggID {
			count++
			if count == data_functions.EggSize {
				lastClickUserID = click.UserID
				break
			}
		}
	}

	if lastClickUserID == 0 {
		return ""
	}

	var user data_models.User
	if err := db.Where("id = ?", lastClickUserID).First(&user).Error; err != nil {
		return ""
	}
	return user.Username
}

func loadScoreboard(db *gorm.DB, gameID uint) []gin.H {
	scores, err := data_models.GetEasterHuntScores(db, gameID)
	if err != nil {
		return []gin.H{}
	}

	result := make([]gin.H, 0, len(scores))
	for _, s := range scores {
		var user data_models.User
		if err := db.Where("id = ?", s.UserID).First(&user).Error; err != nil {
			continue
		}
		result = append(result, gin.H{
			"user_id":  s.UserID,
			"username": user.Username,
			"score":    s.Score,
		})
	}
	return result
}
