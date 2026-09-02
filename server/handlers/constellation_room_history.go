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
//
// ActorUserID is deliberately NOT serialized (arbiter ruling #4847 c27504 R3 /
// c27518): the live graph exposes no author, and an endpoint-scoped history
// that ships the actor would still tell a viewer that a THIRD player set or
// cleared an edge between them and someone else. Stated default, not a
// permanent posture — the open operator question for #4833's next pass is
// "should history say who drew the line, or only that it was drawn?".
type relationshipEventDTO struct {
	ID         uint      `json:"id"`
	Kind       string    `json:"kind"`
	TypeID     uint      `json:"type_id"`
	PairLow    uint      `json:"pair_low"`
	PairHigh   uint      `json:"pair_high"`
	FromUserID uint      `json:"from_user_id"`
	ToUserID   uint      `json:"to_user_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// viewerSeesEvent adapts a log entry onto data_models.ViewerSeesRelationship
// and delegates (#4809, arbiter ruling #4847 c27504 R1, verified c27575
// finding A): ONE predicate decides visibility for the graph and its history,
// so the #4835 reveal path — flipping ViewerSeesRelationship — opens both
// surfaces together and the two can never drift apart. The adapter feeds the
// normalized pair because that is the event row's pair-of-record — on
// undo/redo rows FromUserID/ToUserID are pair-normalized rather than
// direction-preserving (R2) — and a Relationship's From/To are its
// pair-of-record, so the mapping is faithful.
func viewerSeesEvent(e data_models.RelationshipEvent, viewerUserID uint) bool {
	return data_models.ViewerSeesRelationship(
		data_models.Relationship{FromUserID: e.PairLow, ToUserID: e.PairHigh},
		viewerUserID,
	)
}

// GetRoomHistoryHandler returns one oldest-first page of the room's
// relationship-event timeline as seen by the caller.
//
// Cursor pagination for the client's scrollable panel: ?after_id=<id> resumes
// after a previously seen event (0/absent starts at the beginning),
// ?limit=<n> caps the page (default 100, max 500).
//
// The envelope is derived from VISIBLE rows only (arbiter c27575 finding B —
// R3's principle one level out: scoping the row set is not scoping the
// response's fields). A scanned-page cursor would hand a viewer the id,
// ordinal position and time bracket of every event they are not party to —
// one `?limit=1` walk enumerates the whole hidden log. So the handler scans
// forward internally until the visible page is full (or the log is
// exhausted), and `next_after_id`/`has_more` speak only of events the viewer
// may see: `next_after_id` is the last returned event's id, and `has_more` is
// true only when a further VISIBLE event was actually found. The client never
// renders an empty page with has_more=true. The internal scan is bounded by
// the room's log length, not by limit — the price of not leaking, paid
// server-side.
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

	out := make([]relationshipEventDTO, 0, limit)
	hasMore := false
	cursor := uint(afterID)
scan:
	for {
		events, err := data_models.RoomRelationshipEventsPage(db, room.ID, cursor, historyMaxLimit)
		if err != nil {
			log.Printf("Constellations: read event history in room %d: %v", room.ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Error reading history")
			return
		}
		for _, e := range events {
			if !viewerSeesEvent(e, viewerID) {
				continue
			}
			if len(out) == limit {
				// A further visible event exists beyond the full page —
				// the only condition under which has_more may say so.
				hasMore = true
				break scan
			}
			out = append(out, relationshipEventDTO{
				ID: e.ID, Kind: string(e.Kind), TypeID: e.TypeID,
				PairLow: e.PairLow, PairHigh: e.PairHigh,
				FromUserID: e.FromUserID, ToUserID: e.ToUserID,
				CreatedAt: e.CreatedAt,
			})
		}
		if len(events) < historyMaxLimit {
			break // log exhausted
		}
		cursor = events[len(events)-1].ID
	}
	var nextAfterID uint
	if len(out) > 0 {
		nextAfterID = out[len(out)-1].ID
	}
	c.JSON(http.StatusOK, gin.H{
		"events":        out,
		"next_after_id": nextAfterID,
		"has_more":      hasMore,
	})
}
