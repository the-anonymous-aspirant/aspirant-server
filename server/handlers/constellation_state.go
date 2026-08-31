package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Constellations room live-state snapshot (epic #4587, subtask #4600-D1). One
// aggregate GET composing the whole board for a short-poll client. Member-gated
// like the B1/B2 reads it aggregates — the shared graph and dice are visible
// only to a current member of the room.

// roomForStateView resolves the active room from :code and verifies the caller
// is a current member. On failure it has already written the error response.
func roomForStateView(c *gin.Context, db *gorm.DB) (data_models.Room, bool) {
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return data_models.Room{}, false
	}
	code := strings.ToUpper(strings.TrimSpace(c.Param("code")))
	room, ok := data_models.GetActiveRoomByCode(db, code)
	if !ok {
		RespondWithError(c, http.StatusNotFound, "Room not found")
		return data_models.Room{}, false
	}
	if !data_models.IsRoomMember(db, room.ID, userID) {
		RespondWithError(c, http.StatusForbidden, "Only a member of the room may view its state")
		return data_models.Room{}, false
	}
	return room, true
}

// GetRoomStateHandler returns the full board snapshot for the room.
func GetRoomStateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, ok := roomForStateView(c, db)
	if !ok {
		return
	}
	state, err := data_models.BuildRoomState(db, room)
	if err != nil {
		log.Printf("Constellations: build room state for %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading room state")
		return
	}
	c.JSON(http.StatusOK, state)
}
