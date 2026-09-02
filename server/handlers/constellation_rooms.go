package handlers

import (
	"log"
	"net/http"
	"strconv"
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

// Machine-readable refusal reasons for the room lifecycle endpoints (#4806
// ask 1). ErrorDetail.Code is derived from the HTTP status, so it cannot tell a
// full room from a caller already seated elsewhere — both are `conflict` on a
// 409 — and a client that wants to explain WHICH condition blocked would have to
// string-match the human message. These are the stable discriminators it
// branches on instead; the message stays the prose for a human to read.
const (
	reasonRoomNotFound       = "room_not_found"
	reasonRoomEnded          = "room_ended"
	reasonRoomFull           = "room_full"
	reasonAlreadyInGame      = "already_in_game"
	reasonNotInRoom          = "not_in_room"
	reasonInvalidPlayerCount = "invalid_player_count"
)

// roomErrorDetail is the room lifecycle refusal body. It keeps the
// {code, message} envelope every other error response uses (ErrorDetail in
// common.go) and adds, additively:
//   - Reason: the machine-readable condition (see the reason* constants);
//   - ActiveRoomCode: on already_in_game, the room the caller IS in, so the
//     client can name it and link there rather than showing a bare refusal the
//     user cannot act on (#4798);
//   - RoomPlayerCount: on room_full, the room's declared size, so the refusal
//     can say "this game seats 4 and all 4 seats are taken" rather than a bare
//     "room is full" (#4806 ask 1: "a message ... with elucidating details").
//
// Both optional fields are omitted when unresolvable, in which case Message
// stays the bare form.
type roomErrorDetail struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Reason          string `json:"reason"`
	ActiveRoomCode  string `json:"active_room_code,omitempty"`
	RoomPlayerCount int    `json:"room_player_count,omitempty"`
}

// mapRoomError maps a data_models lifecycle error to an HTTP status, a
// machine-readable reason, and a human message. Returns handled=false for an
// unexpected error the caller should treat as a 500.
func mapRoomError(err error) (status int, reason string, msg string, handled bool) {
	switch err {
	case data_models.ErrRoomNotFound:
		return http.StatusNotFound, reasonRoomNotFound, "Room not found", true
	case data_models.ErrRoomEnded:
		return http.StatusNotFound, reasonRoomEnded, "That game has ended", true
	case data_models.ErrRoomFull:
		return http.StatusConflict, reasonRoomFull, "Room is full", true
	case data_models.ErrAlreadyInGame:
		return http.StatusConflict, reasonAlreadyInGame, "You are already in an active game", true
	case data_models.ErrNotInRoom:
		return http.StatusConflict, reasonNotInRoom, "You are not in this room", true
	case data_models.ErrInvalidPlayerCount:
		return http.StatusBadRequest, reasonInvalidPlayerCount, "Player count must be between 2 and 8", true
	default:
		return 0, "", "", false
	}
}

// respondRoomError writes the HTTP response for a data_models lifecycle error.
// It reports whether the error was handled; an unhandled error is the caller's
// 500.
//
// Two reasons carry more than the generic message. already_in_game needs the
// caller's identity — the refusal names the room they are already seated in.
// room_full needs the room behind :code, which the failing call did not return,
// so it is re-read here to state the size that is full.
func respondRoomError(c *gin.Context, db *gorm.DB, userID uint, err error) bool {
	status, reason, msg, handled := mapRoomError(err)
	if !handled {
		return false
	}
	detail := roomErrorDetail{
		Code:    httpStatusToErrorCode(status),
		Reason:  reason,
		Message: msg,
	}
	switch err {
	case data_models.ErrAlreadyInGame:
		if room, ok := data_models.ActiveRoomForUser(db, userID); ok {
			detail.ActiveRoomCode = room.Code
			detail.Message = "You are already in game " + room.Code + " — leave it before starting or joining another."
		}
	case data_models.ErrRoomFull:
		if room, ok := data_models.GetActiveRoomByCode(db, roomCodeParam(c)); ok {
			detail.RoomPlayerCount = room.PlayerCount
			detail.Message = "Room " + room.Code + " is full — all " + strconv.Itoa(room.PlayerCount) + " seats are taken."
		}
	}
	log.Printf("Error response: %d - %s - %s", status, detail.Code, detail.Message)
	c.JSON(status, gin.H{"error": detail})
	return true
}

// roomCodeParam is the normalized :code path parameter — every room route
// resolves the code the same way (upper-cased, trimmed), so the handlers below
// and the refusal enrichment above read it through one helper.
func roomCodeParam(c *gin.Context) string {
	return strings.ToUpper(strings.TrimSpace(c.Param("code")))
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
		if respondRoomError(c, db, userID, err) {
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
	code := roomCodeParam(c)

	room, member, err := data_models.JoinRoom(db, userID, code)
	if err != nil {
		if respondRoomError(c, db, userID, err) {
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
	code := roomCodeParam(c)

	room, err := data_models.LeaveRoom(db, userID, code)
	if err != nil {
		if respondRoomError(c, db, userID, err) {
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
	code := roomCodeParam(c)

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
