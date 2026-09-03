package handlers

import (
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// #4835 — SetRoomSettingsHandler: creator-only, server-side authorization for
// the two transparency toggles. Reuses the rooms_test harness (newRoomHandlerDB,
// roomRouterAs, roomRouterNoAuth, doJSON).

const settingsPath = "/constellations/rooms/:code/settings"

func TestSetRoomSettingsHandler_CreatorSetsToggles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, err := data_models.CreateRoom(db, 7, 4) // creator = 7
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	r := roomRouterAs(db, 7, http.MethodPost, settingsPath, SetRoomSettingsHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+room.Code+"/settings",
		`{"reveal_connections":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("creator set want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, `"reveal_connections":true`) ||
		!strings.Contains(b, `"reveal_cards":false`) {
		t.Fatalf("response toggles wrong: %s", b)
	}

	// A partial second call flips only cards; connections stays on (independent).
	w = doJSON(r, http.MethodPost, "/constellations/rooms/"+room.Code+"/settings",
		`{"reveal_cards":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second set want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, `"reveal_connections":true`) ||
		!strings.Contains(b, `"reveal_cards":true`) {
		t.Fatalf("partial update should leave connections on: %s", b)
	}
}

func TestSetRoomSettingsHandler_NonCreatorForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, err := data_models.CreateRoom(db, 7, 4) // creator = 7
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, _, err := data_models.JoinRoom(db, 8, room.Code); err != nil {
		t.Fatalf("join u8: %v", err)
	}

	// Member 8 is not the creator — 403, and nothing changes.
	r := roomRouterAs(db, 8, http.MethodPost, settingsPath, SetRoomSettingsHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+room.Code+"/settings",
		`{"reveal_connections":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-creator want 403, got %d — body=%s", w.Code, w.Body.String())
	}
	var reloaded data_models.Room
	if err := db.First(&reloaded, room.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.RevealConnections {
		t.Fatalf("refused set must not change the toggle")
	}
}

func TestSetRoomSettingsHandler_UnknownCode404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	r := roomRouterAs(db, 7, http.MethodPost, settingsPath, SetRoomSettingsHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/ZZZZZ/settings",
		`{"reveal_connections":true}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown code want 404, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestSetRoomSettingsHandler_Unauthenticated401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRoomHandlerDB(t)
	defer db.Close()

	room, _, err := data_models.CreateRoom(db, 7, 4)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	r := roomRouterNoAuth(db, http.MethodPost, settingsPath, SetRoomSettingsHandler)
	w := doJSON(r, http.MethodPost, "/constellations/rooms/"+room.Code+"/settings",
		`{"reveal_connections":true}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated want 401, got %d — body=%s", w.Code, w.Body.String())
	}
}
