package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// newHistHandlerDB is newRelHandlerDB plus the RelationshipAction table (C1).
func newHistHandlerDB(t *testing.T) (*gorm.DB, string, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&data_models.Room{}, &data_models.RoomMember{},
		&data_models.RelationshipType{}, &data_models.Relationship{}, &data_models.RelationshipAction{}, &data_models.RelationshipEvent{})
	data_models.SeedRelationshipTypes(db)
	room, _, _ := data_models.CreateRoom(db, 1, 4)
	data_models.JoinRoom(db, 2, room.Code)
	types, _ := data_models.GetRelationshipTypes(db)
	return db, room.Code, types[0].ID
}

func TestHistoryHandlers_SetUndoRedoRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newHistHandlerDB(t)
	defer db.Close()

	setP := "/constellations/rooms/:code/relationships/set"
	setT := "/constellations/rooms/" + code + "/relationships/set"
	// Member 1 sets an edge (records an action for actor 1).
	rs := relRouterAs(db, 1, http.MethodPost, setP, SetRelationshipHandler)
	body := fmt.Sprintf(`{"from_user_id":1,"to_user_id":2,"type_id":%d}`, typeID)
	if w := relDo(rs, http.MethodPost, setT, body); w.Code != http.StatusOK {
		t.Fatalf("set want 200, got %d — %s", w.Code, w.Body.String())
	}

	// History shows one action for member 1.
	rh := relRouterAs(db, 1, http.MethodGet, "/constellations/rooms/:code/relationships/history", GetRelationshipHistoryHandler)
	w := relDo(rh, http.MethodGet, "/constellations/rooms/"+code+"/relationships/history", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"kind":"set"`) {
		t.Fatalf("history want 200 with the set action, got %d — %s", w.Code, w.Body.String())
	}

	// Undo → applied:true, graph empty.
	ru := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/relationships/undo", UndoRelationshipHandler)
	w = relDo(ru, http.MethodPost, "/constellations/rooms/"+code+"/relationships/undo", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applied":true`) || !strings.Contains(w.Body.String(), `"relationships":[]`) {
		t.Fatalf("undo want 200 applied:true empty graph, got %d — %s", w.Code, w.Body.String())
	}
	// Undo again → applied:false (nothing to undo), still 200.
	w = relDo(ru, http.MethodPost, "/constellations/rooms/"+code+"/relationships/undo", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Fatalf("empty-stack undo want 200 applied:false, got %d — %s", w.Code, w.Body.String())
	}

	// Redo → applied:true, edge restored.
	rr := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/relationships/redo", RedoRelationshipHandler)
	w = relDo(rr, http.MethodPost, "/constellations/rooms/"+code+"/relationships/redo", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applied":true`) || !strings.Contains(w.Body.String(), `"from_user_id":1`) {
		t.Fatalf("redo want 200 applied:true with the edge, got %d — %s", w.Code, w.Body.String())
	}
}

func TestHistoryHandlers_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, _ := newHistHandlerDB(t)
	defer db.Close()

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.POST("/constellations/rooms/:code/relationships/undo", UndoRelationshipHandler)
	if w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/undo", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth undo want 401, got %d — %s", w.Code, w.Body.String())
	}
}

func TestHistoryHandlers_NonMemberForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, _ := newHistHandlerDB(t)
	defer db.Close()

	// Caller 9 is Trusted but not a member of the room.
	r := relRouterAs(db, 9, http.MethodPost, "/constellations/rooms/:code/relationships/undo", UndoRelationshipHandler)
	if w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/undo", ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-member undo want 403, got %d — %s", w.Code, w.Body.String())
	}
}

// system_3 #11432: an undo that meets a step whose other player left answers
// 200 with the count and the one line, and still applies the step behind it.
func TestHistoryHandlers_UndoSaysWhatItSkipped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newHistHandlerDB(t)
	defer db.Close()
	data_models.JoinRoom(db, 3, code)
	types, _ := data_models.GetRelationshipTypes(db)
	otherType := types[1].ID

	setP := "/constellations/rooms/:code/relationships/set"
	setT := "/constellations/rooms/" + code + "/relationships/set"
	rs := relRouterAs(db, 1, http.MethodPost, setP, SetRelationshipHandler)
	for _, body := range []string{
		fmt.Sprintf(`{"from_user_id":1,"to_user_id":2,"type_id":%d}`, typeID),
		fmt.Sprintf(`{"from_user_id":1,"to_user_id":3,"type_id":%d}`, typeID),
		fmt.Sprintf(`{"from_user_id":1,"to_user_id":3,"type_id":%d}`, otherType),
	} {
		if w := relDo(rs, http.MethodPost, setT, body); w.Code != http.StatusOK {
			t.Fatalf("set want 200, got %d — %s", w.Code, w.Body.String())
		}
	}

	undoT := "/constellations/rooms/" + code + "/relationships/undo"
	ru := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/relationships/undo", UndoRelationshipHandler)

	if _, err := data_models.LeaveRoom(db, 3, code); err != nil {
		t.Fatalf("leave: %v", err)
	}
	w := relDo(ru, http.MethodPost, undoT, "")
	got := w.Body.String()
	for _, want := range []string{`"applied":true`, `"skipped":1`, `"notice":"skipped: that player left the room"`} {
		if w.Code != http.StatusOK || !strings.Contains(got, want) {
			t.Fatalf("undo past a departed player: want 200 with %s, got %d — %s", want, w.Code, got)
		}
	}
	// The next undo skips nothing: the count is 0 and there is no notice.
	w = relDo(ru, http.MethodPost, undoT, "")
	got = w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(got, `"applied":true`) || !strings.Contains(got, `"skipped":0`) || strings.Contains(got, `"notice"`) {
		t.Fatalf("plain undo: want 200 applied:true skipped:0 and no notice, got %d — %s", w.Code, got)
	}
}

func TestHistoryNotice(t *testing.T) {
	if got := historyNotice(1); got != "skipped: that player left the room" {
		t.Errorf("one step: %q", got)
	}
	if got := historyNotice(3); got != "skipped 3 steps: those players left the room" {
		t.Errorf("three steps: %q", got)
	}
}
