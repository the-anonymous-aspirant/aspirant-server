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
