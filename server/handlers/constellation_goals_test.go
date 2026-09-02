package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newGoalHandlerDB(t *testing.T) (*gorm.DB, string, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&data_models.User{}, &data_models.Room{}, &data_models.RoomMember{},
		&data_models.RelationshipType{}, &data_models.Relationship{},
		&data_models.GoalCard{}, &data_models.PlayerGoal{})
	data_models.SeedGoalCards(db)
	room, _, _ := data_models.CreateRoom(db, 1, 4)
	data_models.JoinRoom(db, 2, room.Code)
	cards, _ := data_models.GetGoalCards(db)
	return db, room.Code, cards[0].ID
}

func TestGetConstellationGoalCardsHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _, _ := newGoalHandlerDB(t)
	defer db.Close()

	r := relRouterAs(db, 1, http.MethodGet, "/constellations/goal-cards", GetConstellationGoalCardsHandler)
	w := relDo(r, http.MethodGet, "/constellations/goal-cards", "")
	if w.Code != http.StatusOK {
		t.Fatalf("goal-cards want 200, got %d — %s", w.Code, w.Body.String())
	}
	var body struct {
		GoalCards []data_models.GoalCard `json:"goal_cards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — %s", err, w.Body.String())
	}
	if len(body.GoalCards) != 16 {
		t.Fatalf("want 16 goal cards, got %d", len(body.GoalCards))
	}
}

func TestSetGoalHandler_Member(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, cardID := newGoalHandlerDB(t)
	defer db.Close()

	r := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/goal/set", SetGoalHandler)
	body := fmt.Sprintf(`{"goal_card_id": %d}`, cardID)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/goal/set", body)
	if w.Code != http.StatusOK {
		t.Fatalf("set goal want 200, got %d — %s", w.Code, w.Body.String())
	}
	// Persisted for the caller.
	room, _ := data_models.GetActiveRoomByCode(db, code)
	if got, ok := data_models.GetPlayerGoal(db, room.ID, 1); !ok || got.ID != cardID {
		t.Fatalf("goal not persisted for caller: (%+v, %v)", got, ok)
	}
}

func TestSetGoalHandler_UnknownCard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, _ := newGoalHandlerDB(t)
	defer db.Close()

	r := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/goal/set", SetGoalHandler)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/goal/set", `{"goal_card_id": 99999}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown card want 400, got %d — %s", w.Code, w.Body.String())
	}
}

func TestSetGoalHandler_NonMemberForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, cardID := newGoalHandlerDB(t)
	defer db.Close()

	// Caller 9 is Trusted but not a member of the room.
	r := relRouterAs(db, 9, http.MethodPost, "/constellations/rooms/:code/goal/set", SetGoalHandler)
	body := fmt.Sprintf(`{"goal_card_id": %d}`, cardID)
	w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/goal/set", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-member set want 403, got %d — %s", w.Code, w.Body.String())
	}
}

func TestClearGoalHandler_Idempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, code, cardID := newGoalHandlerDB(t)
	defer db.Close()
	room, _ := data_models.GetActiveRoomByCode(db, code)
	data_models.SetPlayerGoal(db, room, 1, cardID)

	r := relRouterAs(db, 1, http.MethodPost, "/constellations/rooms/:code/goal/clear", ClearGoalHandler)
	if w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/goal/clear", ""); w.Code != http.StatusOK {
		t.Fatalf("clear want 200, got %d — %s", w.Code, w.Body.String())
	}
	if _, ok := data_models.GetPlayerGoal(db, room.ID, 1); ok {
		t.Fatalf("goal still set after clear")
	}
	// Clearing again still 200 (idempotent).
	if w := relDo(r, http.MethodPost, "/constellations/rooms/"+code+"/goal/clear", ""); w.Code != http.StatusOK {
		t.Fatalf("second clear want 200, got %d — %s", w.Code, w.Body.String())
	}
}
