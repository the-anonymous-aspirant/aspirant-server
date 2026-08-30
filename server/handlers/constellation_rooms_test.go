package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newRoomHandlerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.Room{}, &data_models.RoomMember{})
	return db
}

// roomRouterAs builds a single-route engine as an authenticated Trusted caller.
func roomRouterAs(db *gorm.DB, userID uint, method, path string, h gin.HandlerFunc) *gin.Engine {
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

// roomRouterNoAuth builds a route with the db but no user_id, to exercise 401.
func roomRouterNoAuth(db *gorm.DB, method, path string, h gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.Handle(method, path, h)
	return r
}

func doJSON(r *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCreateRoomHandler_Created(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	r := roomRouterAs(db, 1, http.MethodPost, "/constellations/rooms", CreateRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms", `{"player_count":4}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code"`) || !strings.Contains(body, `"slot":1`) {
		t.Errorf("response missing code/slot: %s", body)
	}
}

func TestCreateRoomHandler_BadPlayerCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	r := roomRouterAs(db, 1, http.MethodPost, "/constellations/rooms", CreateRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms", `{"player_count":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestCreateRoomHandler_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	r := roomRouterNoAuth(db, http.MethodPost, "/constellations/rooms", CreateRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms", `{"player_count":4}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestJoinRoomHandler_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	r := roomRouterAs(db, 2, http.MethodPost, "/constellations/rooms/:code/join", JoinRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/ZZZZZ/join", ``)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestJoinRoomHandler_AlreadyInGame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	// user 1 creates room A, user 1 is already in it; joining any room again 409s.
	roomA, _, _ := data_models.CreateRoom(db, 1, 4)
	roomB, _, _ := data_models.CreateRoom(db, 2, 4)
	_ = roomA

	r := roomRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/join", JoinRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+roomB.Code+"/join", ``)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestJoinRoomHandler_LowercaseCodeNormalised(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, _ := data_models.CreateRoom(db, 1, 4)
	r := roomRouterAs(db, 2, http.MethodPost, "/constellations/rooms/:code/join", JoinRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+strings.ToLower(room.Code)+"/join", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for lowercased code, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestGetRoomHandler_OccupancyAndMembers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, _ := data_models.CreateRoom(db, 1, 4)
	data_models.JoinRoom(db, 2, room.Code)

	r := roomRouterAs(db, 1, http.MethodGet, "/constellations/rooms/:code", GetRoomHandler)
	w := doJSON(r, http.MethodGet, "/constellations/rooms/"+room.Code, ``)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"occupancy":2`) {
		t.Errorf("occupancy not 2 in body: %s", body)
	}
}

func TestLeaveRoomHandler_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, _ := data_models.CreateRoom(db, 1, 4)
	r := roomRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/leave", LeaveRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+room.Code+"/leave", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"occupancy":0`) {
		t.Errorf("occupancy not 0 after last leave: %s", w.Body.String())
	}
}
