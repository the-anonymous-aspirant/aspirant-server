package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Constellations room settings API (#4835). The two creator-only transparency
// toggles — reveal others' connections, reveal others' relationship cards.
// "Only the room creator can set" is a server-side authorization rule (like the
// endpoint-only write rule of #4834), enforced in data_models.SetRoomReveal, not
// a hidden client control.

// roomSettingsRequest carries the two toggles as POINTERS so a partial update
// touches only the toggle named in the body — the two are independent (a room
// may reveal lines while keeping cards hidden). A nil field is left unchanged.
type roomSettingsRequest struct {
	RevealConnections *bool `json:"reveal_connections"`
	RevealCards       *bool `json:"reveal_cards"`
}

// roomSettingsDTO echoes the toggle state after a successful update.
type roomSettingsDTO struct {
	RevealConnections bool `json:"reveal_connections"`
	RevealCards       bool `json:"reveal_cards"`
}

// SetRoomSettingsHandler updates the room's creator-only transparency toggles.
// It resolves the active room from :code, then defers the creator check to
// data_models.SetRoomReveal so the authorization is one rule in one place.
func SetRoomSettingsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(c.Param("code")))
	room, ok := data_models.GetActiveRoomByCode(db, code)
	if !ok {
		RespondWithError(c, http.StatusNotFound, "Room not found")
		return
	}

	var req roomSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	updated, err := data_models.SetRoomReveal(db, room, userID, req.RevealConnections, req.RevealCards)
	if err != nil {
		if err == data_models.ErrNotRoomCreator {
			RespondWithError(c, http.StatusForbidden, "Only the room creator may change room settings")
			return
		}
		log.Printf("Constellations: set room settings for %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating room settings")
		return
	}

	c.JSON(http.StatusOK, roomSettingsDTO{
		RevealConnections: updated.RevealConnections,
		RevealCards:       updated.RevealCards,
	})
}
