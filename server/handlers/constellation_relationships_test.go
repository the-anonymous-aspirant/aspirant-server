package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// newRelHandlerDB seeds a room (creator=1) with a second member (2), the six
// relationship types, and returns the db, the room code, and a valid type id.
func newRelHandlerDB(t *testing.T) (*gorm.DB, string, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&data_models.Room{}, &data_models.RoomMember{}, &data_models.RelationshipType{}, &data_models.Relationship{}, &data_models.RelationshipAction{})
	data_models.SeedRelationshipTypes(db)
	room, _, _ := data_models.CreateRoom(db, 1, 4)
	data_models.JoinRoom(db, 2, room.Code)
	types, _ := data_models.GetRelationshipTypes(db)
	return db, room.Code, types[0].ID
}

// relRouterAs builds a single-route engine as a Trusted caller with userID.
func relRouterAs(db *gorm.DB, userID uint, method, path string, h gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("role", "Trusted")
		c.Set("user_id", userID)
		c.Next()
	})
	r.Handle(method, path, h)
	return r
}

func relDo(r *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSetRelationshipHandler_Member(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	r := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/relationships/set", SetRelationshipHandler)
	body := fmt.Sprintf(`{"from_user_id":1,"to_user_id":2,"type_id":%d}`, typeID)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/set", body)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"from_user_id":1`) {
		t.Errorf("response missing edge: %s", w.Body.String())
	}
}

func TestSetRelationshipHandler_NonMemberForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	// Caller 9 is Trusted but not a member of the room.
	r := relRouterAs(db, 9, http.MethodPost, "/constellations/rooms/:code/relationships/set", SetRelationshipHandler)
	body := fmt.Sprintf(`{"from_user_id":1,"to_user_id":2,"type_id":%d}`, typeID)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/set", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for non-member, got %d — %s", w.Code, w.Body.String())
	}
}

func TestSetRelationshipHandler_BadType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, _ := newRelHandlerDB(t)
	defer db.Close()

	r := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/relationships/set", SetRelationshipHandler)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/set", `{"from_user_id":1,"to_user_id":2,"type_id":99999}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad type, got %d — %s", w.Code, w.Body.String())
	}
}

func TestSetRelationshipHandler_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.POST("/constellations/rooms/:code/relationships/set", SetRelationshipHandler)
	body := fmt.Sprintf(`{"from_user_id":1,"to_user_id":2,"type_id":%d}`, typeID)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/relationships/set", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d — %s", w.Code, w.Body.String())
	}
}

func TestClearAndListRelationshipHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, typeID := newRelHandlerDB(t)
	defer db.Close()

	// Seed one edge directly.
	room, _ := data_models.GetActiveRoomByCode(db, code)
	data_models.SetRelationship(db, room, 1, 2, typeID)

	// List shows it.
	rl := relRouterAs(db, 1, http.MethodGet, "/constellations/rooms/:code/relationships", GetRelationshipsHandler)
	w := relDo(rl, http.MethodGet, "/constellations/rooms/"+code+"/relationships", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"from_user_id":1`) {
		t.Fatalf("list want 200 with edge, got %d — %s", w.Code, w.Body.String())
	}

	// Clear it.
	rc := relRouterAs(db, 2, http.MethodPost, "/constellations/rooms/:code/relationships/clear", ClearRelationshipHandler)
	w = relDo(rc, http.MethodPost, "/constellations/rooms/"+code+"/relationships/clear", `{"from_user_id":1,"to_user_id":2}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear want 200, got %d — %s", w.Code, w.Body.String())
	}

	// List is now empty.
	w = relDo(rl, http.MethodGet, "/constellations/rooms/"+code+"/relationships", "")
	if !strings.Contains(w.Body.String(), `"relationships":[]`) {
		t.Errorf("graph not empty after clear: %s", w.Body.String())
	}
}
