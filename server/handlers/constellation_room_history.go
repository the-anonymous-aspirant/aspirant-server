package handlers

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Room relationship-event history read (epic #4833, subtask #4847-A1).
//
// Reads the #4829-A3 append-only log (constellation_relationship_events) —
// the room's full ordered set/clear timeline, including the events undo/redo
// append — NOT the #4599 per-player capped undo stack that
// GET .../relationships/history serves.

const (
	historyDefaultLimit = 100
	historyMaxLimit     = 500
)

// relationshipEventDTO is one entry of the room's shared timeline.
type relationshipEventDTO struct {
	ID          uint      `json:"id"`
	Kind        string    `json:"kind"`
	TypeID      uint      `json:"type_id"`
	PairLow     uint      `json:"pair_low"`
	PairHigh    uint      `json:"pair_high"`
	FromUserID  uint      `json:"from_user_id"`
	ToUserID    uint      `json:"to_user_id"`
	ActorUserID uint      `json:"actor_user_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// viewerSeesEvent mirrors data_models.ViewerSeesRelationship for log entries
// (#4809, operator c27159 "for now, private"): an event is visible only to the
// endpoints of its edge. It keys on the normalized pair rather than
// from/to because a clear event carries zeroes there.
func viewerSeesEvent(e data_models.RelationshipEvent, viewerUserID uint) bool {
	return e.PairLow == viewerUserID || e.PairHigh == viewerUserID
}

// GetRoomHistoryHandler returns one oldest-first page of the room's
// relationship-event timeline as seen by the caller.
//
// Cursor pagination for the client's scrollable panel: ?after_id=<id> resumes
// after a previously seen event (0/absent starts at the beginning),
// ?limit=<n> caps the page (default 100, max 500). next_after_id / has_more
// describe the SCANNED page, not the visible one: the privacy filter runs on
// the way out (never in the query — see RoomRelationshipEventsPage), so a
// page may return fewer visible events than it scanned while has_more is
// still true. A client keeps paging until has_more is false.
func GetRoomHistoryHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	room, viewerID, ok := roomForGraphEdit(c, db)
	if !ok {
		return
	}

	afterID, err := strconv.ParseUint(c.DefaultQuery("after_id", "0"), 10, 64)
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "after_id must be a non-negative integer")
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(historyDefaultLimit)))
	if err != nil || limit < 1 {
		RespondWithError(c, http.StatusBadRequest, "limit must be a positive integer")
		return
	}
	if limit > historyMaxLimit {
		limit = historyMaxLimit
	}

	events, err := data_models.RoomRelationshipEventsPage(db, room.ID, uint(afterID), limit)
	if err != nil {
		log.Printf("Constellations: read event history in room %d: %v", room.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading history")
		return
	}

	out := make([]relationshipEventDTO, 0, len(events))
	for _, e := range events {
		if !viewerSeesEvent(e, viewerID) {
			continue
		}
		out = append(out, relationshipEventDTO{
			ID: e.ID, Kind: string(e.Kind), TypeID: e.TypeID,
			PairLow: e.PairLow, PairHigh: e.PairHigh,
			FromUserID: e.FromUserID, ToUserID: e.ToUserID,
			ActorUserID: e.ActorUserID, CreatedAt: e.CreatedAt,
		})
	}
	var nextAfterID uint
	if len(events) > 0 {
		nextAfterID = events[len(events)-1].ID
	}
	c.JSON(http.StatusOK, gin.H{
		"events":        out,
		"next_after_id": nextAfterID,
		"has_more":      len(events) == limit,
	})
}
