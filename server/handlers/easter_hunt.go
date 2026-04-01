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
	easterHuntInitialBudget = 5
	easterHuntDefaultLock   = "2026-04-05T16:00:00Z" // 18:00 CEST = 16:00 UTC
)

// computeClickBudget returns remaining clicks and when the next one is added.
func computeClickBudget(gameCreatedAt time.Time, clicksUsed int) (remaining int, nextRefillAt time.Time) {
	now := time.Now().UTC()
	hoursElapsed := int(now.Sub(gameCreatedAt).Hours())
	totalBudget := easterHuntInitialBudget + hoursElapsed
	remaining = totalBudget - clicksUsed
	if remaining < 0 {
		remaining = 0
	}
	nextRefillAt = gameCreatedAt.Add(time.Duration(hoursElapsed+1) * time.Hour)
	return
}

// ---------- Public ----------

func GetEasterHuntStateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	// Load eggs from DB
	eggs, err := data_models.GetEasterHuntEggs(db, game.ID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving eggs")
		return
	}

	// Load all clicks
	clicks, err := data_models.GetEasterHuntClicks(db, game.ID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving clicks")
		return
	}

	// Load egg cells to build grid lookup
	eggCells, _ := data_models.GetEasterHuntEggCells(db, game.ID)
	cellToEgg := make(map[[2]int]int) // (x,y) → egg_index
	for _, cell := range eggCells {
		cellToEgg[[2]int{cell.X, cell.Y}] = cell.EggIndex
	}

	// Build username lookup
	usernames := loadUsernames(db, clicks)

	// Build revealed squares list
	revealed := make([]gin.H, 0, len(clicks))
	for _, click := range clicks {
		eggIndex := -1
		if idx, ok := cellToEgg[[2]int{click.X, click.Y}]; ok {
			eggIndex = idx
		}
		revealed = append(revealed, gin.H{
			"x":        click.X,
			"y":        click.Y,
			"egg_id":   eggIndex,
			"user_id":  click.UserID,
			"username": usernames[click.UserID],
		})
	}

	// Build egg progress from DB eggs
	// Count revealed cells per egg from clicks
	revealedPerEgg := make(map[int]int)
	for _, click := range clicks {
		if idx, ok := cellToEgg[[2]int{click.X, click.Y}]; ok {
			revealedPerEgg[idx]++
		}
	}

	eggProgress := make([]gin.H, 0, len(eggs))
	for _, egg := range eggs {
		revCount := revealedPerEgg[egg.EggIndex]
		if revCount > egg.TotalCells {
			revCount = egg.TotalCells
		}
		completed := egg.CompletedByUserID != nil
		var completedBy interface{} = nil
		if completed && egg.CompletedByUserID != nil {
			var user data_models.User
			if err := db.Where("id = ?", *egg.CompletedByUserID).First(&user).Error; err == nil {
				completedBy = user.Username
			}
		}
		eggProgress = append(eggProgress, gin.H{
			"egg_id":       egg.EggIndex,
			"color":        egg.Color,
			"squares":      egg.TotalCells,
			"revealed":     revCount,
			"completed":    completed,
			"completed_by": completedBy,
		})
	}

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
			"eggs":     eggProgress,
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

	// Check click budget (admins bypass)
	role, _ := c.Get("role")
	isAdmin := role == "Admin"
	if !isAdmin {
		score, _ := data_models.GetOrCreateEasterHuntScore(db, game.ID, userID)
		clicksUsed := 0
		if score != nil {
			clicksUsed = score.ClicksUsed
		}
		remaining, nextRefill := computeClickBudget(game.CreatedAt, clicksUsed)
		if remaining <= 0 {
			untilRefill := nextRefill.Sub(now)
			mins := int(untilRefill.Minutes())
			secs := int(untilRefill.Seconds()) % 60
			respondError(c, http.StatusTooManyRequests, "no_clicks",
				fmt.Sprintf("No clicks remaining. Next click in %dm %ds", mins, secs))
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
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("x and y must be between 0 and %d", data_functions.BoardWidth-1))
		return
	}

	// Load egg cell lookup for this game
	eggCells, _ := data_models.GetEasterHuntEggCells(db, game.ID)
	cellToEgg := make(map[[2]int]int)
	for _, cell := range eggCells {
		cellToEgg[[2]int{cell.X, cell.Y}] = cell.EggIndex
	}

	// Area reveal: all cells within ±RevealRadius of the clicked cell
	radius := data_functions.RevealRadius
	var newClicks []data_models.EasterHuntClick
	for dx := -radius; dx <= radius; dx++ {
		for dy := -radius; dy <= radius; dy++ {
			cx, cy := body.X+dx, body.Y+dy
			if cx < 0 || cx >= data_functions.BoardWidth || cy < 0 || cy >= data_functions.BoardHeight {
				continue
			}
			click, err := data_models.CreateEasterHuntClick(db, game.ID, userID, cx, cy)
			if err != nil {
				continue // already revealed, skip
			}
			newClicks = append(newClicks, *click)
		}
	}

	if len(newClicks) == 0 {
		respondError(c, http.StatusConflict, "conflict", "All squares in this area already revealed")
		return
	}

	// Deduct one click from the user's budget
	if !isAdmin {
		if err := data_models.IncrementClicksUsed(db, game.ID, userID); err != nil {
			log.Printf("Error incrementing clicks_used: %v", err)
		}
	}

	// Determine which eggs were touched by newly revealed cells
	touchedEggs := make(map[int]bool)
	for _, cl := range newClicks {
		if idx, ok := cellToEgg[[2]int{cl.X, cl.Y}]; ok {
			touchedEggs[idx] = true
		}
	}

	// Check completion for each touched egg using DB counts
	completedEggs := []int{}
	for eggIndex := range touchedEggs {
		eggs, _ := data_models.GetEasterHuntEggs(db, game.ID)
		var egg *data_models.EasterHuntEgg
		for i := range eggs {
			if eggs[i].EggIndex == eggIndex {
				egg = &eggs[i]
				break
			}
		}
		if egg == nil || egg.CompletedByUserID != nil {
			continue // already completed or not found
		}

		revealedCount := data_models.CountRevealedCellsForEgg(db, game.ID, eggIndex)
		if revealedCount >= egg.TotalCells {
			if data_models.MarkEggCompleted(db, game.ID, eggIndex, userID) {
				completedEggs = append(completedEggs, eggIndex)
				if err := data_models.UpsertEasterHuntScore(db, game.ID, userID); err != nil {
					log.Printf("Error upserting score for egg %d: %v", eggIndex, err)
				}
			}
		}
	}

	// Build revealed cells response
	revealedCells := make([]gin.H, 0, len(newClicks))
	for _, cl := range newClicks {
		eggIndex := -1
		if idx, ok := cellToEgg[[2]int{cl.X, cl.Y}]; ok {
			eggIndex = idx
		}
		revealedCells = append(revealedCells, gin.H{
			"x":      cl.X,
			"y":      cl.Y,
			"egg_id": eggIndex,
		})
	}

	// Compute remaining budget for response
	budgetRemaining := 999 // admin default
	nextRefillAt := time.Time{}
	nextRefillSeconds := 0
	if !isAdmin {
		score, _ := data_models.GetOrCreateEasterHuntScore(db, game.ID, userID)
		clicksUsed := 0
		if score != nil {
			clicksUsed = score.ClicksUsed
		}
		budgetRemaining, nextRefillAt = computeClickBudget(game.CreatedAt, clicksUsed)
		if budgetRemaining <= 0 {
			nextRefillSeconds = int(nextRefillAt.Sub(time.Now().UTC()).Seconds())
			if nextRefillSeconds < 0 {
				nextRefillSeconds = 0
			}
		}
	}
	RespondWithSuccess(c, gin.H{
		"x":                    body.X,
		"y":                    body.Y,
		"revealed":             revealedCells,
		"revealed_count":       len(newClicks),
		"eggs_completed":       completedEggs,
		"eggs_completed_count": len(completedEggs),
		"clicks_remaining":     budgetRemaining,
		"next_refill_at":       nextRefillAt.UTC().Format(time.RFC3339),
		"next_refill_seconds":  nextRefillSeconds,
	}, "Area revealed")
}

func GetEasterHuntCooldownHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID := c.MustGet("user_id").(uint)

	game, err := data_models.GetActiveEasterHuntGame(db)
	if err != nil {
		RespondWithError(c, http.StatusNotFound, "No active game")
		return
	}

	role, _ := c.Get("role")
	if role == "Admin" {
		RespondWithSuccess(c, gin.H{
			"clicks_remaining":    999,
			"next_refill_at":      time.Time{}.UTC().Format(time.RFC3339),
			"next_refill_seconds": 0,
		}, "Budget status retrieved")
		return
	}

	score, _ := data_models.GetOrCreateEasterHuntScore(db, game.ID, userID)
	clicksUsed := 0
	if score != nil {
		clicksUsed = score.ClicksUsed
	}
	remaining, nextRefillAt := computeClickBudget(game.CreatedAt, clicksUsed)

	nextRefillSeconds := 0
	if remaining <= 0 {
		nextRefillSeconds = int(nextRefillAt.Sub(time.Now().UTC()).Seconds())
		if nextRefillSeconds < 0 {
			nextRefillSeconds = 0
		}
	}

	RespondWithSuccess(c, gin.H{
		"clicks_remaining":    remaining,
		"next_refill_at":      nextRefillAt.UTC().Format(time.RFC3339),
		"next_refill_seconds": nextRefillSeconds,
	}, "Budget status retrieved")
}

// ---------- Admin ----------

func PostEasterHuntResetHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var body struct {
		LockAt string `json:"lock_at"`
		Seed   *int64 `json:"seed"`
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

	var seed int64
	if body.Seed != nil {
		seed = *body.Seed
	} else {
		seed = int64(rand.New(rand.NewSource(time.Now().UnixNano())).Int63())
	}

	game, err := data_models.CreateEasterHuntGame(db, seed, lockAt)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Error creating game")
		return
	}

	// Generate board and persist eggs + cells to DB
	board := data_functions.GenerateEggBoard(game.Seed)
	for _, egg := range board.Eggs {
		dbEgg := data_models.EasterHuntEgg{
			GameID:     game.ID,
			EggIndex:   egg.ID,
			Color:      egg.Color,
			TotalCells: len(egg.Squares),
		}
		if err := data_models.CreateEasterHuntEgg(db, &dbEgg); err != nil {
			log.Printf("Error persisting egg %d: %v", egg.ID, err)
		}

		cells := make([]data_models.EasterHuntEggCell, 0, len(egg.Squares))
		for _, sq := range egg.Squares {
			cells = append(cells, data_models.EasterHuntEggCell{
				GameID:   game.ID,
				EggIndex: egg.ID,
				X:        sq.X,
				Y:        sq.Y,
			})
		}
		if err := data_models.CreateEasterHuntEggCells(db, cells); err != nil {
			log.Printf("Error persisting egg %d cells: %v", egg.ID, err)
		}
	}

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

	// Read eggs and cells from DB
	dbEggs, _ := data_models.GetEasterHuntEggs(db, game.ID)
	allCells, _ := data_models.GetEasterHuntEggCells(db, game.ID)

	// Group cells by egg index
	cellsByEgg := make(map[int][]gin.H)
	for _, cell := range allCells {
		cellsByEgg[cell.EggIndex] = append(cellsByEgg[cell.EggIndex], gin.H{"x": cell.X, "y": cell.Y})
	}

	eggs := make([]gin.H, 0, len(dbEggs))
	for _, egg := range dbEggs {
		eggs = append(eggs, gin.H{
			"egg_id":  egg.EggIndex,
			"color":   egg.Color,
			"squares": cellsByEgg[egg.EggIndex],
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
