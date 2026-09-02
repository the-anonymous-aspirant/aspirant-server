package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Constellations goal-card API (epic #4807, subtask #4807-A1). A read of the
// victory-condition deck (for the dictionary surface), and set/clear of the
// CALLER's own goal in a room. The selection is private: it is surfaced only in
// the caller's own /state (BuildRoomState), never another player's, so there is
// no read-another-player's-goal route by design.

// GetConstellationGoalCardsHandler returns the goal-card deck (16 victory-
// condition cards) with the text and predicate key stored per card, so the
// frontend renders the dictionary from the server rather than hard-coding it.
func GetConstellationGoalCardsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	cards, err := data_models.GetGoalCards(db)
	if err != nil {
		log.Printf("Error retrieving goal cards: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving goal cards")
		return
	}
	c.JSON(http.StatusOK, gin.H{"goal_cards": cards})
}

type setGoalRequest struct {
	GoalCardID uint `json:"goal_card_id"`
}

// roomForGoalSelect resolves the active room from :code and verifies the caller
// is a current member. The caller is always the acting player — a goal is only
// ever set for oneself. On failure it has already written the error response.
func roomForGoalSelect(c *gin.Context, db *gorm.DB) (data_models.Room, uint, bool) {
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return data_models.Room{}, 0, false
	}
	code := strings.ToUpper(strings.TrimSpace(c.Param("code")))
	room, ok := data_models.GetActiveRoomByCode(db, code)
	if !ok {
		RespondWithError(c, http.StatusNotFound, "Room not found")
		return data_models.Room{}, 0, false
	}
	if !data_models.IsRoomMember(db, room.ID, userID) {
		RespondWithError(c, http.StatusForbidden, "Only a member of the room may select a goal")
		return data_models.Room{}, 0, false
	}
	return room, userID, true
}

func mapGoalError(err error) (int, string, bool) {
	switch err {
	case data_models.ErrGoalNotMember:
		return http.StatusForbidden, "Only a member of the room may select a goal", true
	case data_models.ErrGoalUnknownCard:
		return http.StatusBadRequest, "Unknown goal card", true
	default:
		return 0, "", false
	}
}

// goalDTO is the caller's own selected goal, echoed back on set.
type goalDTO struct {
	GoalCardID uint   `json:"goal_card_id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
}

// SetGoalHandler records (or replaces) the caller's own goal in the room.
func SetGoalHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGoalSelect(c, db)
	if !ok {
		return
	}
	var req setGoalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	pg, err := data_models.SetPlayerGoal(db, room, actor, req.GoalCardID)
	if err != nil {
		if status, msg, handled := mapGoalError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: set goal in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error setting goal")
		return
	}
	card, _ := data_models.GetGoalCardByID(db, pg.GoalCardID)
	c.JSON(http.StatusOK, goalDTO{GoalCardID: pg.GoalCardID, Code: card.Code, Name: card.Name})
}

// ClearGoalHandler removes the caller's own goal in the room (idempotent).
func ClearGoalHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGoalSelect(c, db)
	if !ok {
		return
	}
	if err := data_models.ClearPlayerGoal(db, room, actor); err != nil {
		if status, msg, handled := mapGoalError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: clear goal in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error clearing goal")
		return
	}
	c.JSON(http.StatusOK, gin.H{"cleared": true})
}
