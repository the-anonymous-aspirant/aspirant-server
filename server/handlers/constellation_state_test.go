package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newStateHandlerDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.Room{}, &data_models.RoomMember{},
		&data_models.RelationshipType{}, &data_models.Relationship{},
		&data_models.RelationshipAction{}, &data_models.DiceRoll{}, &data_models.ConstellationProfile{})
	data_models.SeedRelationshipTypes(db)
	room, _, _ := data_models.CreateRoom(db, 1, 4)
	data_models.JoinRoom(db, 2, room.Code)
	return db, room.Code
}

func TestGetRoomStateHandler_Member(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newStateHandlerDB(t)
	defer db.Close()

	// Seed one relationship so the composed shape is non-trivial.
	types, _ := data_models.GetRelationshipTypes(db)
	room, _ := data_models.GetActiveRoomByCode(db, code)
	data_models.SetRelationshipWithHistory(db, room, 1, 1, 2, types[0].ID)

	r := relRouterAs(db, 1, http.MethodGet, "/constellations/rooms/:code/state", GetRoomStateHandler)
	w := relDo(r, http.MethodGet, "/constellations/rooms/"+code+"/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("state want 200, got %d — %s", w.Code, w.Body.String())
	}
	var st data_models.RoomState
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("state shape did not decode into RoomState: %v — %s", err, w.Body.String())
	}
	if st.Code != code || st.Occupancy != 2 || len(st.Members) != 2 || len(st.Relationships) != 1 {
		t.Fatalf("composed state wrong: %+v", st)
	}
	if st.Relationships[0].Colour == "" {
		t.Fatalf("relationship missing type colour: %+v", st.Relationships[0])
	}
}

func TestGetRoomStateHandler_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newStateHandlerDB(t)
	defer db.Close()

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	r.GET("/constellations/rooms/:code/state", GetRoomStateHandler)
	if w := relDo(r, http.MethodGet, "/constellations/rooms/"+code+"/state", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth state want 401, got %d — %s", w.Code, w.Body.String())
	}
}

func TestGetRoomStateHandler_NonMemberForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code := newStateHandlerDB(t)
	defer db.Close()

	// Caller 9 is Trusted but not a member.
	r := relRouterAs(db, 9, http.MethodGet, "/constellations/rooms/:code/state", GetRoomStateHandler)
	if w := relDo(r, http.MethodGet, "/constellations/rooms/"+code+"/state", ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-member state want 403, got %d — %s", w.Code, w.Body.String())
	}
}
