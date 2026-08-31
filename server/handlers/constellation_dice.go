package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Constellations server-authoritative dice-roll sync (epic #4587, subtask
// #4597-B2). A roll is resolved server-side and persisted; every viewer reads
// the same resolved faces + nonce and animates locally. Routes sit behind the
// Trusted/Admin member-app gate; additionally only a current member of the room
// may roll or read its dice.

// diceResponse is the resolved roll state shared across all viewers. nonce is
// the roll's identity (the row id); a client re-animates when it changes.
type diceResponse struct {
	Faces    []int  `json:"faces"`
	Nonce    uint   `json:"nonce"`
	RolledAt string `json:"rolled_at,omitempty"`
}

// callerInRoom checks the caller is authenticated and a current member of the
// active room named by :code. Uses A1's RoomMembers rather than a shared
// IsRoomMember helper (B1 introduces that symbol; a second copy would collide
// at merge). On failure it writes the error response and returns ok=false.
func diceRoomForCaller(c *gin.Context, db *gorm.DB) (data_models.Room, bool) {
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
	members, _ := data_models.RoomMembers(db, room.ID)
	for _, m := range members {
		if m.UserID == userID {
			return room, true
		}
	}
	RespondWithError(c, http.StatusForbidden, "Only a member of the room may use its dice")
	return data_models.Room{}, false
}

func rollToResponse(roll data_models.DiceRoll) diceResponse {
	return diceResponse{
		Faces:    roll.FaceValues(),
		Nonce:    roll.ID,
		RolledAt: roll.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// RollDiceHandler resolves a new roll server-side and returns it.
func RollDiceHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, ok := diceRoomForCaller(c, db)
	if !ok {
		return
	}
	roll, err := data_models.RollDice(db, room)
	if err != nil {
		log.Printf("Constellations: roll dice in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error rolling dice")
		return
	}
	c.JSON(http.StatusOK, rollToResponse(roll))
}

// GetDiceHandler returns the room's current resolved roll, or an empty roll if
// the dice have never been rolled.
func GetDiceHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, ok := diceRoomForCaller(c, db)
	if !ok {
		return
	}
	roll, exists := data_models.CurrentRoll(db, room.ID)
	if !exists {
		c.JSON(http.StatusOK, diceResponse{Faces: []int{}, Nonce: 0})
		return
	}
	c.JSON(http.StatusOK, rollToResponse(roll))
}
