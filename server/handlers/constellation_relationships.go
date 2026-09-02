package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Constellations relationship-graph edit API (epic #4587, subtask #4596-B1).
// Thin handlers over data_models: set (upsert) an edge between two members,
// clear it, and read the room's shared graph. All routes sit behind the
// member-app Trusted/Admin gate; additionally, only a current member of the
// room may edit or read its graph.

type setRelationshipRequest struct {
	FromUserID uint `json:"from_user_id"`
	ToUserID   uint `json:"to_user_id"`
	TypeID     uint `json:"type_id"`
}

type clearRelationshipRequest struct {
	FromUserID uint `json:"from_user_id"`
	ToUserID   uint `json:"to_user_id"`
}

type relationshipDTO struct {
	FromUserID uint `json:"from_user_id"`
	ToUserID   uint `json:"to_user_id"`
	TypeID     uint `json:"type_id"`
}

func relationshipToDTO(r data_models.Relationship) relationshipDTO {
	return relationshipDTO{FromUserID: r.FromUserID, ToUserID: r.ToUserID, TypeID: r.TypeID}
}

// mapRelationshipError maps a data_models edit error to an HTTP status.
func mapRelationshipError(err error) (int, string, bool) {
	switch err {
	case data_models.ErrRelSameMember:
		return http.StatusBadRequest, "A relationship needs two distinct members", true
	case data_models.ErrRelNotMember:
		return http.StatusBadRequest, "Both endpoints must be current members of the room", true
	case data_models.ErrRelInvalidType:
		return http.StatusBadRequest, "Unknown relationship type", true
	case data_models.ErrRelNoActiveEdge:
		return http.StatusNotFound, "No active relationship for that pair", true
	default:
		return 0, "", false
	}
}

// roomForGraphEdit resolves the active room from the :code path param and
// verifies the caller is a current member of it. Returns the room and true on
// success; on failure it has already written the error response.
func roomForGraphEdit(c *gin.Context, db *gorm.DB) (data_models.Room, uint, bool) {
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
		RespondWithError(c, http.StatusForbidden, "Only a member of the room may edit its graph")
		return data_models.Room{}, 0, false
	}
	return room, userID, true
}

// SetRelationshipHandler upserts the edge between two members of the room and
// records the edit on the caller's undo stack (C1).
func SetRelationshipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	var req setRelationshipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	rel, err := data_models.SetRelationshipWithHistory(db, room, actor, req.FromUserID, req.ToUserID, req.TypeID)
	if err != nil {
		if status, msg, handled := mapRelationshipError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: set relationship in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error setting relationship")
		return
	}
	c.JSON(http.StatusOK, relationshipToDTO(rel))
}

// ClearRelationshipHandler clears the edge between two members and records the
// edit on the caller's undo stack (C1).
func ClearRelationshipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	var req clearRelationshipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := data_models.ClearRelationshipWithHistory(db, room, actor, req.FromUserID, req.ToUserID); err != nil {
		if status, msg, handled := mapRelationshipError(err); handled {
			RespondWithError(c, status, msg)
			return
		}
		log.Printf("Constellations: clear relationship in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error clearing relationship")
		return
	}
	c.JSON(http.StatusOK, gin.H{"cleared": true})
}

// GetRelationshipsHandler returns the room's shared graph (active edges).
func GetRelationshipsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, viewerID, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	rels, err := data_models.RoomRelationships(db, room.ID)
	if err != nil {
		log.Printf("Constellations: list relationships in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading relationships")
		return
	}
	// Scoped to the caller's own connections, exactly as the /state aggregate
	// is (#4806 ask 2). This endpoint is the second door onto the same data:
	// the board reads edges through /state, but any member with a session can
	// call this one, so scoping only /state would leave the leak open behind a
	// front door that looked fixed.
	out := make([]relationshipDTO, 0, len(rels))
	for _, r := range rels {
		if !data_models.ViewerSeesRelationship(r, viewerID) {
			continue
		}
		out = append(out, relationshipToDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"relationships": out})
}

// roomGraphDTO reads the room's active edges as DTOs (shared by the graph read
// and by undo/redo, which return the resulting graph so the client re-renders).
func roomGraphDTO(db *gorm.DB, roomID uint) ([]relationshipDTO, error) {
	rels, err := data_models.RoomRelationships(db, roomID)
	if err != nil {
		return nil, err
	}
	out := make([]relationshipDTO, 0, len(rels))
	for _, r := range rels {
		out = append(out, relationshipToDTO(r))
	}
	return out, nil
}

// relationshipActionDTO is one entry of a player's history.
type relationshipActionDTO struct {
	ID         uint   `json:"id"`
	Kind       string `json:"kind"`
	PairLow    uint   `json:"pair_low"`
	PairHigh   uint   `json:"pair_high"`
	TypeID     uint   `json:"type_id"`
	FromUserID uint   `json:"from_user_id"`
	ToUserID   uint   `json:"to_user_id"`
	Undone     bool   `json:"undone"`
}

// applyUndoRedo runs an undo/redo step and returns the resulting graph. A benign
// empty-stack condition (ErrNothingToUndo / ErrNothingToRedo) is not an error:
// it responds 200 with the unchanged graph and applied=false, so a UI "back" at
// the start of history is a no-op, not a failure.
func applyUndoRedo(c *gin.Context, db *gorm.DB, room data_models.Room, err error, empty error) {
	applied := true
	if err == empty {
		applied = false
	} else if err != nil {
		log.Printf("Constellations: undo/redo in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error applying history")
		return
	}
	out, gerr := roomGraphDTO(db, room.ID)
	if gerr != nil {
		log.Printf("Constellations: read graph after history in room %d: %v", room.ID, gerr)
		RespondWithError(c, http.StatusInternalServerError, "Error reading relationships")
		return
	}
	c.JSON(http.StatusOK, gin.H{"relationships": out, "applied": applied})
}

// UndoRelationshipHandler reverts the caller's most recent relationship edit.
func UndoRelationshipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	err := data_models.UndoRelationship(db, room, actor)
	applyUndoRedo(c, db, room, err, data_models.ErrNothingToUndo)
}

// RedoRelationshipHandler re-applies the caller's most recently undone edit.
func RedoRelationshipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	err := data_models.RedoRelationship(db, room, actor)
	applyUndoRedo(c, db, room, err, data_models.ErrNothingToRedo)
}

// GetRelationshipHistoryHandler returns the caller's own retained edit history.
func GetRelationshipHistoryHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, actor, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	actions, err := data_models.PlayerHistory(db, room.ID, actor)
	if err != nil {
		log.Printf("Constellations: read history in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading history")
		return
	}
	out := make([]relationshipActionDTO, 0, len(actions))
	for _, a := range actions {
		out = append(out, relationshipActionDTO{
			ID: a.ID, Kind: string(a.Kind), PairLow: a.PairLow, PairHigh: a.PairHigh,
			TypeID: a.TypeID, FromUserID: a.FromUserID, ToUserID: a.ToUserID, Undone: a.Undone,
		})
	}
	c.JSON(http.StatusOK, gin.H{"history": out})
}
