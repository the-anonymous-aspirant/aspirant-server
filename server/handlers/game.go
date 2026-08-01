package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"aspirant-online/server/data_functions"
	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// GetLongestWordsHandler handles the request for getting the longest words from rows and columns
func GetLongestWordsHandler(c *gin.Context) {
	var boardRequest struct {
		Board    [][]string `json:"board" binding:"required"`
		Language string     `json:"language"`
	}

	if err := c.ShouldBindJSON(&boardRequest); err != nil {
		log.Printf("Invalid board data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid board data")
		return
	}

	// Validate board dimensions
	if len(boardRequest.Board) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Board cannot be empty")
		return
	}

	longestWordsInRows, rowDefinitions, longestWordsInCols, colDefinitions :=
		data_functions.GetLongestWordsWithDefinitionsFromBoard(boardRequest.Board, boardRequest.Language)

	log.Printf("Processed word weaver board: found %d row words and %d column words",
		len(longestWordsInRows), len(longestWordsInCols))

	RespondWithSuccess(c, gin.H{
		"longest_words_in_rows": longestWordsInRows,
		"row_definitions":       rowDefinitions,
		"longest_words_in_cols": longestWordsInCols,
		"col_definitions":       colDefinitions,
	}, "Board processed successfully")
}

// SaveGameScoreHandler handles saving a universal game score
func SaveGameScoreHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var body struct {
		Game     string          `json:"game"`
		Mode     string          `json:"mode"`
		Score    int             `json:"score"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		log.Printf("Invalid game score data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid score data")
		return
	}

	if body.Game == "" {
		RespondWithError(c, http.StatusBadRequest, "Game name is required")
		return
	}
	if body.Score < 0 {
		RespondWithError(c, http.StatusBadRequest, "Score cannot be negative")
		return
	}

	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	score := data_models.GameScore{
		UserID:   int(userID.(uint)),
		Game:     body.Game,
		Mode:     body.Mode,
		Score:    body.Score,
		Metadata: body.Metadata,
	}

	log.Printf("Saving game score %d for user_id: %d, game: %s, mode: %s",
		score.Score, score.UserID, score.Game, score.Mode)

	if err := score.PostGameScore(db); err != nil {
		log.Printf("Error saving game score: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving score")
		return
	}

	RespondWithSuccess(c, score, "Score saved successfully")
}

// GetGameScoresHandler handles retrieving leaderboard scores for a game
func GetGameScoresHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	game := c.Query("game")
	if game == "" {
		RespondWithError(c, http.StatusBadRequest, "Game parameter is required")
		return
	}

	mode := c.Query("mode")
	page, pageSize := parsePagination(c)
	offset := (page - 1) * pageSize

	log.Printf("Retrieving game scores for game: %s, mode: %s, page: %d, page_size: %d", game, mode, page, pageSize)

	// Build base query
	query := db.Model(&data_models.GameScore{}).Where("game = ?", game)
	if mode != "" {
		query = query.Where("mode = ?", mode)
	}

	// Get total count
	var total int64
	if err := query.Count(&total).Error; err != nil {
		log.Printf("Error counting game scores: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving scores")
		return
	}

	// Fetch paginated scores
	var scores []data_models.GameScore
	if err := query.Order("score DESC").Offset(offset).Limit(pageSize).Find(&scores).Error; err != nil {
		log.Printf("Error retrieving game scores: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving scores")
		return
	}

	// Publish the display name, never the login username: this route is
	// unauthenticated (routes.go), so emitting user.Username would hand
	// anonymous callers a valid username list (security-finding #3094).
	// Batch-resolve to avoid an N+1 across the page of scores.
	userIDs := make([]uint, 0, len(scores))
	for _, s := range scores {
		userIDs = append(userIDs, uint(s.UserID))
	}
	displayNames := data_models.CurrentDisplayNames(db, userIDs)

	var result []gin.H
	for _, s := range scores {
		name := displayNames[uint(s.UserID)]
		if name == "" {
			// Orphan score with no resolvable user — skip, matching prior behaviour.
			log.Printf("Skipping score with unresolvable user ID %d", s.UserID)
			continue
		}

		result = append(result, gin.H{
			"username":   name,
			"score":      s.Score,
			"mode":       s.Mode,
			"metadata":   s.Metadata,
			"created_at": s.CreatedAt,
		})
	}

	log.Printf("Retrieved %d game scores for game: %s", len(result), game)
	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    result,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}
