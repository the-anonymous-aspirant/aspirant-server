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

// SetRelationshipHandler upserts the edge between two members of the room.
func SetRelationshipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, _, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	var req setRelationshipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	rel, err := data_models.SetRelationship(db, room, req.FromUserID, req.ToUserID, req.TypeID)
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

// ClearRelationshipHandler clears the edge between two members.
func ClearRelationshipHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, _, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	var req clearRelationshipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := data_models.ClearRelationship(db, room, req.FromUserID, req.ToUserID); err != nil {
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
	room, _, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}
	rels, err := data_models.RoomRelationships(db, room.ID)
	if err != nil {
		log.Printf("Constellations: list relationships in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading relationships")
		return
	}
	out := make([]relationshipDTO, 0, len(rels))
	for _, r := range rels {
		out = append(out, relationshipToDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"relationships": out})
}
