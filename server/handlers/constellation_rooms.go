package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Constellations rooms & membership lifecycle API (epic #4587, subtask
// #4593-A1). Thin handlers over data_models: create a room, join by code,
// leave, and read room state (occupancy + slots). All routes sit behind the
// member-app Trusted/Admin gate in routes.go.

type createRoomRequest struct {
	PlayerCount int `json:"player_count"`
}

// roomMemberDTO is the seat view the board needs to place an avatar. Game
// username / avatar enrichment is the profile (A3) and snapshot (D1) child's
// job; A1 exposes the seat identity only.
type roomMemberDTO struct {
	UserID uint `json:"user_id"`
	Slot   int  `json:"slot"`
}

// roomResponse is the room's public lifecycle state.
type roomResponse struct {
	Code        string          `json:"code"`
	PlayerCount int             `json:"player_count"`
	Status      string          `json:"status"`
	Occupancy   int             `json:"occupancy"`
	Slot        int             `json:"slot,omitempty"` // the caller's own slot, when applicable
	Members     []roomMemberDTO `json:"members,omitempty"`
}

func membersDTO(db *gorm.DB, roomID uint) []roomMemberDTO {
	members, err := data_models.RoomMembers(db, roomID)
	if err != nil {
		return nil
	}
	out := make([]roomMemberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, roomMemberDTO{UserID: m.UserID, Slot: m.Slot})
	}
	return out
}

// mapRoomError maps a data_models lifecycle error to an HTTP status + message.
// Returns (status, msg, true) when handled, or (_, _, false) for an unexpected
// error the caller should treat as a 500.
func mapRoomError(err error) (int, string, bool) {
	switch err {
	case data_models.ErrRoomNotFound:
		return http.StatusNotFound, "Room not found", true
	case data_models.ErrRoomFull:
		return http.StatusConflict, "Room is full", true
	case data_models.ErrAlreadyInGame:
		return http.StatusConflict, "You are already in an active game", true
	case data_models.ErrNotInRoom:
		return http.StatusConflict, "You are not in this room", true
	case data_models.ErrInvalidPlayerCount:
		return http.StatusBadRequest, "Player count must be between 2 and 8", true
	default:
		return 0, "", false
	}
}

// CreateRoomHandler creates a room with the requested player count, generates a
// unique code, and seats the caller in slot 1.
func CreateRoomHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	var req createRoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	room, member, err := data_models.CreateRoom(db, userID, req.PlayerCount)
	if err != nil {
		if status, msg, handled := mapRoomError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: create room for user %d: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating room")
		return
	}

	c.JSON(http.StatusCreated, roomResponse{
		Code:        room.Code,
		PlayerCount: room.PlayerCount,
		Status:      room.Status,
		Occupancy:   data_models.RoomOccupancy(db, room.ID),
		Slot:        member.Slot,
		Members:     membersDTO(db, room.ID),
	})
}

// JoinRoomHandler seats the caller in the room named by the :code path
// parameter, assigning the next free slot.
func JoinRoomHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(c.Param("code")))

	room, member, err := data_models.JoinRoom(db, userID, code)
	if err != nil {
		if status, msg, handled := mapRoomError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: join room %q for user %d: %v", code, userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error joining room")
		return
	}

	c.JSON(http.StatusOK, roomResponse{
		Code:        room.Code,
		PlayerCount: room.PlayerCount,
		Status:      room.Status,
		Occupancy:   data_models.RoomOccupancy(db, room.ID),
		Slot:        member.Slot,
		Members:     membersDTO(db, room.ID),
	})
}

// LeaveRoomHandler marks the caller left. When the last member leaves, the
// room is slated for deletion.
func LeaveRoomHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(c.Param("code")))

	room, err := data_models.LeaveRoom(db, userID, code)
	if err != nil {
		if status, msg, handled := mapRoomError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: leave room %q for user %d: %v", code, userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error leaving room")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":      room.Code,
		"occupancy": data_models.RoomOccupancy(db, room.ID),
	})
}

// GetRoomHandler returns the room's live lifecycle state: occupancy and the
// current seat assignments.
func GetRoomHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	if _, ok := callerUserID(c); !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(c.Param("code")))

	room, ok := data_models.GetActiveRoomByCode(db, code)
	if !ok {
		RespondWithError(c, http.StatusNotFound, "Room not found")
		return
	}

	c.JSON(http.StatusOK, roomResponse{
		Code:        room.Code,
		PlayerCount: room.PlayerCount,
		Status:      room.Status,
		Occupancy:   data_models.RoomOccupancy(db, room.ID),
		Members:     membersDTO(db, room.ID),
	})
}
