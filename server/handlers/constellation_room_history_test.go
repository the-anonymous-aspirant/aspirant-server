package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Room relationship-event history endpoint (#4847): ordered timeline, #4809
// viewer scoping asserted in both directions, cursor pagination.

type roomHistoryResponse struct {
	Events []struct {
		ID          uint   `json:"id"`
		Kind        string `json:"kind"`
		TypeID      uint   `json:"type_id"`
		PairLow     uint   `json:"pair_low"`
		PairHigh    uint   `json:"pair_high"`
		ActorUserID uint   `json:"actor_user_id"`
		CreatedAt   string `json:"created_at"`
	} `json:"events"`
	NextAfterID uint `json:"next_after_id"`
	HasMore     bool `json:"has_more"`
}

func historyGet(db *gorm.DB, userID uint, code, query string) (*roomHistoryResponse, int, string) {
	r := relRouterAs(db, userID, http.MethodGet, "/constellations/rooms/:code/history", GetRoomHistoryHandler)
	w := relDo(r, http.MethodGet, "/constellations/rooms/"+code+"/history"+query, "")
	var resp roomHistoryResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return &resp, w.Code, w.Body.String()
}

func historySet(t *testing.T, db *gorm.DB, actor, from, to, typeID uint, code string) {
	t.Helper()
	r := relRouterAs(db, actor, http.MethodPost, "/constellations/rooms/:code/relationships/set", SetRelationshipHandler)
	body := fmt.Sprintf(`{"from_user_id":%d,"to_user_id":%d,"type_id":%d}`, from, to, typeID)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/set", body)
	if w.Code != http.StatusOK {
		t.Fatalf("seed set %d-%d: want 200, got %d — %s", from, to, w.Code, w.Body.String())
	}
}

func historyClear(t *testing.T, db *gorm.DB, actor, from, to uint, code string) {
	t.Helper()
	r := relRouterAs(db, actor, http.MethodPost, "/constellations/rooms/:code/relationships/clear", ClearRelationshipHandler)
	body := fmt.Sprintf(`{"from_user_id":%d,"to_user_id":%d}`, from, to)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/clear", body)
	if w.Code != http.StatusOK {
		t.Fatalf("seed clear %d-%d: want 200, got %d — %s", from, to, w.Code, w.Body.String())
	}
}

func TestGetRoomHistoryHandler_OrderedTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	historySet(t, db, 1, 1, 2, typeID, code)
	historyClear(t, db, 1, 1, 2, code)
	historySet(t, db, 1, 1, 2, typeID, code)

	resp, status, body := historyGet(db, 1, code, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", status, body)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("want 3 events, got %d — %s", len(resp.Events), body)
	}
	kinds := []string{resp.Events[0].Kind, resp.Events[1].Kind, resp.Events[2].Kind}
	if kinds[0] != "set" || kinds[1] != "clear" || kinds[2] != "set" {
		t.Errorf("want set/clear/set oldest-first, got %v", kinds)
	}
	if !(resp.Events[0].ID < resp.Events[1].ID && resp.Events[1].ID < resp.Events[2].ID) {
		t.Errorf("events not in ascending id order: %s", body)
	}
	if resp.Events[0].CreatedAt == "" {
		t.Errorf("events must carry created_at for rendering: %s", body)
	}
	if resp.HasMore {
		t.Errorf("3 events under the default limit must not report has_more: %s", body)
	}
}

func TestGetRoomHistoryHandler_UndoAppendsToTimeline(t *testing.T) {
	// The log is append-only: an undo does not erase the set, it appends the
	// restoring event, so the timeline keeps the full story.
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	historySet(t, db, 1, 1, 2, typeID, code)
	r := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/relationships/undo", UndoRelationshipHandler)
	if w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/undo", ""); w.Code != http.StatusOK {
		t.Fatalf("undo: want 200, got %d — %s", w.Code, w.Body.String())
	}

	resp, status, body := historyGet(db, 1, code, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", status, body)
	}
	if len(resp.Events) != 2 || resp.Events[0].Kind != "set" || resp.Events[1].Kind != "clear" {
		t.Fatalf("want set then clear after undo, got %s", body)
	}
}

func TestGetRoomHistoryHandler_ViewerScoping(t *testing.T) {
	// #4809 scoping, both halves: the viewer sees their own edge's events AND
	// does not see events for an edge they are not party to — while the
	// underlying model read still returns the whole timeline (the filter
	// lives in the serializer, so goal detection keeps the full log).
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()
	if _, _, err := data_models.JoinRoom(db, 3, code); err != nil {
		t.Fatalf("join member 3: %v", err)
	}

	historySet(t, db, 1, 1, 2, typeID, code) // hidden from 3
	historySet(t, db, 1, 1, 3, typeID, code) // visible to 3

	resp, status, body := historyGet(db, 3, code, "")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", status, body)
	}
	if len(resp.Events) != 1 || resp.Events[0].PairHigh != 3 {
		t.Fatalf("viewer 3 must see exactly their 1-3 event, got %s", body)
	}
	// Devtools-class leak check on the raw body: the hidden 1-2 pair must not
	// appear in any serialized field.
	if strings.Contains(body, `"pair_high":2`) {
		t.Errorf("viewer 3's response leaks the 1-2 edge: %s", body)
	}
	events, err := data_models.RoomRelationshipEvents(db, roomIDByCode(t, db, code))
	if err != nil || len(events) != 2 {
		t.Fatalf("underlying log must keep both events (filter belongs in the serializer), got %d, err %v", len(events), err)
	}
}

func TestGetRoomHistoryHandler_Pagination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	for i := 0; i < 5; i++ {
		historySet(t, db, 1, 1, 2, typeID, code)
		historyClear(t, db, 1, 1, 2, code)
	}

	var got []uint
	after, pages := uint(0), 0
	for {
		resp, status, body := historyGet(db, 1, code, fmt.Sprintf("?after_id=%d&limit=4", after))
		if status != http.StatusOK {
			t.Fatalf("page after %d: want 200, got %d — %s", after, status, body)
		}
		for _, e := range resp.Events {
			got = append(got, e.ID)
		}
		pages++
		if !resp.HasMore {
			break
		}
		after = resp.NextAfterID
	}
	if len(got) != 10 || pages != 3 {
		t.Fatalf("want all 10 events over 3 pages of 4, got %d events over %d pages", len(got), pages)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("paged walk not strictly ascending: %v", got)
		}
	}
}

func TestGetRoomHistoryHandler_BadCursorRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, _ := newRelHandlerDB(t)
	defer db.Close()

	if _, status, _ := historyGet(db, 1, code, "?after_id=nope"); status != http.StatusBadRequest {
		t.Errorf("non-numeric after_id: want 400, got %d", status)
	}
	if _, status, _ := historyGet(db, 1, code, "?limit=0"); status != http.StatusBadRequest {
		t.Errorf("zero limit: want 400, got %d", status)
	}
}

func TestGetRoomHistoryHandler_NonMemberForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	historySet(t, db, 1, 1, 2, typeID, code)
	if _, status, _ := historyGet(db, 9, code, ""); status != http.StatusForbidden {
		t.Errorf("non-member: want 403, got %d", status)
	}
}

func roomIDByCode(t *testing.T, db *gorm.DB, code string) uint {
	t.Helper()
	room, ok := data_models.GetActiveRoomByCode(db, code)
	if !ok {
		t.Fatalf("room %s not found", code)
	}
	return room.ID
}
