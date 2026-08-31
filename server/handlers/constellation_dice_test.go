package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newDiceHandlerDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&data_models.Room{}, &data_models.RoomMember{}, &data_models.DiceRoll{})
	room, _, _ := data_models.CreateRoom(db, 1, 4)
	data_models.JoinRoom(db, 2, room.Code)
	return db, room.Code
}

func diceRouterAs(db *gorm.DB, userID uint, method, path string, h gin.HandlerFunc) *gin.Engine {
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

func diceDo(r *gin.Engine, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type diceBody struct {
	Faces []int `json:"faces"`
	Nonce uint  `json:"nonce"`
}

func TestRollDiceHandler_Member(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newDiceHandlerDB(t)
	defer db.Close()

	r := diceRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/dice/roll", RollDiceHandler)
	w := diceDo(r, http.MethodPost, "/constellations/rooms/"+code+"/dice/roll")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", w.Code, w.Body.String())
	}
	var b diceBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("bad json: %v — %s", err, w.Body.String())
	}
	if len(b.Faces) != data_models.DiceCount || b.Nonce == 0 {
		t.Errorf("roll response = %+v, want %d faces and a non-zero nonce", b, data_models.DiceCount)
	}
	if b.Faces[0] < 1 || b.Faces[0] > data_models.DiceFaces {
		t.Errorf("face out of range: %d", b.Faces[0])
	}
}

func TestRollDiceHandler_NonMemberForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newDiceHandlerDB(t)
	defer db.Close()

	r := diceRouterAs(db, 9, http.MethodPost, "/constellations/rooms/:code/dice/roll", RollDiceHandler)
	w := diceDo(r, http.MethodPost, "/constellations/rooms/"+code+"/dice/roll")
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for non-member, got %d — %s", w.Code, w.Body.String())
	}
}

func TestGetDiceHandler_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newDiceHandlerDB(t)
	defer db.Close()

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.GET("/constellations/rooms/:code/dice", GetDiceHandler)
	w := diceDo(r, http.MethodGet, "/constellations/rooms/"+code+"/dice")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d — %s", w.Code, w.Body.String())
	}
}

func TestGetDiceHandler_NeverRolled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newDiceHandlerDB(t)
	defer db.Close()

	r := diceRouterAs(db, 1, http.MethodGet, "/constellations/rooms/:code/dice", GetDiceHandler)
	w := diceDo(r, http.MethodGet, "/constellations/rooms/"+code+"/dice")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"faces":[]`) || !strings.Contains(w.Body.String(), `"nonce":0`) {
		t.Errorf("never-rolled body = %s, want empty faces + nonce 0", w.Body.String())
	}
}

// Acceptance: two successive polls (by two different members) after one roll
// return the same faces + nonce.
func TestDiceConvergesAcrossCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newDiceHandlerDB(t)
	defer db.Close()

	// Member 1 rolls.
	rr := diceRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/dice/roll", RollDiceHandler)
	rollW := diceDo(rr, http.MethodPost, "/constellations/rooms/"+code+"/dice/roll")
	var rolled diceBody
	json.Unmarshal(rollW.Body.Bytes(), &rolled)

	read := func(uid uint) diceBody {
		gr := diceRouterAs(db, uid, http.MethodGet, "/constellations/rooms/:code/dice", GetDiceHandler)
		w := diceDo(gr, http.MethodGet, "/constellations/rooms/"+code+"/dice")
		var b diceBody
		json.Unmarshal(w.Body.Bytes(), &b)
		return b
	}

	a := read(1)
	b := read(2)
	if a.Nonce != rolled.Nonce || b.Nonce != rolled.Nonce {
		t.Errorf("nonce not stable across callers: rolled=%d a=%d b=%d", rolled.Nonce, a.Nonce, b.Nonce)
	}
	if len(a.Faces) == 0 || a.Faces[0] != rolled.Faces[0] || b.Faces[0] != rolled.Faces[0] {
		t.Errorf("faces not stable across callers: rolled=%v a=%v b=%v", rolled.Faces, a.Faces, b.Faces)
	}
}
