package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

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

	r := roomRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/join", JoinRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+roomB.Code+"/join", ``)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d — body=%s", w.Code, w.Body.String())
	}
	// #4798: the refusal names the room the caller is stuck in, both as a
	// machine-readable field and inside the message the client renders, so the
	// user can navigate there to leave it. Guards against the bare-string form.
	assertNamesActiveRoom(t, w.Body.String(), roomA.Code)
	if strings.Contains(w.Body.String(), roomB.Code) {
		t.Errorf("refusal names the room joined (%s) rather than the active one: %s", roomB.Code, w.Body.String())
	}
}

// assertNamesActiveRoom checks an ErrAlreadyInGame 409 body carries the active
// room code in both the additive active_room_code field and the human message.
func assertNamesActiveRoom(t *testing.T, body, activeCode string) {
	t.Helper()
	var parsed struct {
		Error struct {
			Code           string `json:"code"`
			Message        string `json:"message"`
			ActiveRoomCode string `json:"active_room_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal 409 body: %v — body=%s", err, body)
	}
	if parsed.Error.Code != "conflict" {
		t.Errorf("error.code = %q, want conflict — body=%s", parsed.Error.Code, body)
	}
	if parsed.Error.ActiveRoomCode != activeCode {
		t.Errorf("active_room_code = %q, want %q — body=%s", parsed.Error.ActiveRoomCode, activeCode, body)
	}
	if !strings.Contains(parsed.Error.Message, activeCode) {
		t.Errorf("message %q does not name the active room %q", parsed.Error.Message, activeCode)
	}
}

// Creating a second room while already seated is refused with the same
// room-naming 409 the join arm returns (#4798).
func TestCreateRoomHandler_AlreadyInGameNamesRoom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	active, _, err := data_models.CreateRoom(db, 1, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	r := roomRouterAs(db, 1, http.MethodPost, "/constellations/rooms", CreateRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms", `{"player_count":4}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d — body=%s", w.Code, w.Body.String())
	}
	assertNamesActiveRoom(t, w.Body.String(), active.Code)
}

// A room-lifecycle error that is NOT ErrAlreadyInGame keeps the plain
// {code, message} envelope — active_room_code is scoped to the one refusal.
func TestJoinRoomHandler_NotFoundCarriesNoRoomCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	r := roomRouterAs(db, 2, http.MethodPost, "/constellations/rooms/:code/join", JoinRoomHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/ZZZZZ/join", ``)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d — body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "active_room_code") {
		t.Errorf("404 body carries active_room_code: %s", w.Body.String())
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

// #4785 (HTTP journey): a second player can join the shared code after the solo
// creator has left. Before the fix, the creator's Leave slated a never-played
// room and the join 404'd — the operator's exact report (create -> leave -> join).
func TestJoinRoomHandler_SucceedsAfterSoloCreatorLeft(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, err := data_models.CreateRoom(db, 7, 4) // creator, slot 1
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	leave := roomRouterAs(db, 7, http.MethodPost, "/constellations/rooms/:code/leave", LeaveRoomHandler)
	if w := doJSON(leave, http.MethodPost, "/constellations/rooms/"+room.Code+"/leave", ``); w.Code != http.StatusOK {
		t.Fatalf("solo creator leave: want 200, got %d — body=%s", w.Code, w.Body.String())
	}

	join := roomRouterAs(db, 8, http.MethodPost, "/constellations/rooms/:code/join", JoinRoomHandler)
	w := doJSON(join, http.MethodPost, "/constellations/rooms/"+room.Code+"/join", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("second user join after solo creator left: want 200, got %d — body=%s", w.Code, w.Body.String())
	}
}

// logoutRouter wires the db the way the app's router-level middleware does, so
// the public LogoutHandler can reach it via c.MustGet("db"). No auth middleware:
// /logout is public and recovers identity from the token itself.
func logoutRouter(db *gorm.DB) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.POST("/logout", LogoutHandler)
	return r
}

// Logging out while seated in a room releases the one-game-at-a-time lock
// (#4778): the handler reads the user from the still-valid token and leaves the
// room, so a fresh login can create/join again. This is the wiring the model
// test (TestLeaveAllActiveRoomsReleasesLock) cannot cover — cookie/token →
// userID → leave-all.
func TestLogoutHandler_LeavesActiveRoom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, err := data_models.CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	token, err := middleware.GenerateToken(7, "Trusted")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	logoutRouter(db).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200 — body=%s", w.Code, w.Body.String())
	}
	// #4785: a never-played solo room must NOT be slated by the creator's logout
	// — the shared code stays joinable. What logout MUST do is release the
	// one-game lock, proven below by a successful create after logout.
	if _, ok := data_models.GetActiveRoomByCode(db, room.Code); !ok {
		t.Errorf("room %q was slated by the sole creator's logout (regresses #4785)", room.Code)
	}
	if _, _, err := data_models.CreateRoom(db, 7, 4); err != nil {
		t.Errorf("user could not create a game after logout: %v (lock not released)", err)
	}
}

// An anonymous logout (no token) still succeeds and touches no room state — the
// public logout contract is unchanged for callers who are not identifiable.
func TestLogoutHandler_AnonymousLeavesRoomsUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, err := data_models.CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil) // no token
	w := httptest.NewRecorder()
	logoutRouter(db).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("anonymous logout status = %d, want 200", w.Code)
	}
	if _, ok := data_models.GetActiveRoomByCode(db, room.Code); !ok {
		t.Errorf("room %q was slated by an anonymous logout that could not identify a member", room.Code)
	}
}
