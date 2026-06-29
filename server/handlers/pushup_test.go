package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func setupPushupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.PushupEntry{}, &data_models.PushupMilestone{})
	t.Cleanup(func() { db.Close() })
	return db
}

func setupPushupRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		if userID > 0 {
			c.Set("user_id", userID)
		}
		c.Next()
	})
	r.GET("/pushups/entries", GetPushupEntriesHandler)
	r.PATCH("/pushups/entries/:date", PatchPushupEntryHandler)
	r.GET("/pushups/milestones", GetPushupMilestonesHandler)
	return r
}

// withPushupClock temporarily swaps the package clock for tests.
func withPushupClock(t *testing.T, fixed time.Time) {
	t.Helper()
	original := pushupNow
	pushupNow = func() time.Time { return fixed }
	t.Cleanup(func() { pushupNow = original })
}

func TestGetPushupEntries_GapFills60Days(t *testing.T) {
	db := setupPushupDB(t)
	router := setupPushupRouter(db, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/pushups/entries", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Entries []pushupEntryDTO `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(resp.Entries) != 60 {
		t.Fatalf("expected 60 gap-filled entries, got %d", len(resp.Entries))
	}
	if resp.Entries[0].Date != "2026-07-01" {
		t.Errorf("expected first date 2026-07-01, got %s", resp.Entries[0].Date)
	}
	if resp.Entries[59].Date != "2026-08-29" {
		t.Errorf("expected last date 2026-08-29, got %s", resp.Entries[59].Date)
	}
	for i, e := range resp.Entries {
		if e.Count != nil {
			t.Errorf("entry %d: expected nil count on empty db, got %v", i, *e.Count)
		}
	}
}

func TestPatchPushupEntry_WithinEditWindowPersists(t *testing.T) {
	db := setupPushupDB(t)
	withPushupClock(t, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 42)

	body := `{"count": 25}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/pushups/entries/2026-07-15", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	saved, err := data_models.GetPushupEntryByDate(db, time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("error reading back: %v", err)
	}
	if saved == nil || saved.Count == nil || *saved.Count != 25 {
		t.Fatalf("expected saved count 25, got %+v", saved)
	}
	if saved.UpdatedBy != 42 {
		t.Errorf("expected UpdatedBy=42, got %d", saved.UpdatedBy)
	}
}

func TestPatchPushupEntry_NeighborInEditWindowAllowed(t *testing.T) {
	db := setupPushupDB(t)
	// today=2026-07-15 → editable: 13, 14, 15, 16, 17
	withPushupClock(t, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 1)

	for _, d := range []string{"2026-07-13", "2026-07-14", "2026-07-15", "2026-07-16", "2026-07-17"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PATCH", "/pushups/entries/"+d, bytes.NewBufferString(`{"count": 10}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("date %s expected 200, got %d: %s", d, w.Code, w.Body.String())
		}
	}
}

func TestPatchPushupEntry_OutsideEditWindowReturns403(t *testing.T) {
	db := setupPushupDB(t)
	withPushupClock(t, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 1)

	for _, d := range []string{"2026-07-12", "2026-07-18", "2026-08-01"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PATCH", "/pushups/entries/"+d, bytes.NewBufferString(`{"count": 10}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("date %s expected 403, got %d: %s", d, w.Code, w.Body.String())
		}
	}
}

func TestPatchPushupEntry_BeforeChallengeWindowReturns400(t *testing.T) {
	db := setupPushupDB(t)
	withPushupClock(t, time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/pushups/entries/2026-06-29", bytes.NewBufferString(`{"count": 10}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("pre-challenge date expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchPushupEntry_AfterChallengeWindowReturns400(t *testing.T) {
	db := setupPushupDB(t)
	withPushupClock(t, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/pushups/entries/2026-08-30", bytes.NewBufferString(`{"count": 10}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("post-challenge date expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchPushupEntry_NegativeCountReturns400(t *testing.T) {
	db := setupPushupDB(t)
	withPushupClock(t, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/pushups/entries/2026-07-15", bytes.NewBufferString(`{"count": -3}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for negative count, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchPushupEntry_BadDateFormatReturns400(t *testing.T) {
	db := setupPushupDB(t)
	router := setupPushupRouter(db, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/pushups/entries/not-a-date", bytes.NewBufferString(`{"count": 1}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad date, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeedPushupMilestones_PopulatesCanonical(t *testing.T) {
	db := setupPushupDB(t)
	data_models.SeedPushupMilestones(db)

	router := setupPushupRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/pushups/milestones", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Milestones []data_models.PushupMilestone `json:"milestones"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Milestones) < 6 {
		t.Fatalf("expected at least 6 seeded milestones, got %d", len(resp.Milestones))
	}
	required := map[int]string{
		59:   "Lika många armhävningar som din ålder — nice!",
		100:  "100 nere, 900 kvar!",
		500:  "500 — halvvägs!",
		555:  "555 — ditt favoritnummer!",
		1000: "Mål! Du klarade det!",
	}
	got := make(map[int]string, len(resp.Milestones))
	for _, m := range resp.Milestones {
		got[m.CumulativeCount] = m.MessageSv
	}
	for k, v := range required {
		if got[k] != v {
			t.Errorf("milestone %d: expected %q, got %q", k, v, got[k])
		}
	}
}

func TestSeedPushupMilestones_Idempotent(t *testing.T) {
	db := setupPushupDB(t)
	data_models.SeedPushupMilestones(db)
	data_models.SeedPushupMilestones(db)

	var first int
	db.Model(&data_models.PushupMilestone{}).Count(&first)
	data_models.SeedPushupMilestones(db)
	var second int
	db.Model(&data_models.PushupMilestone{}).Count(&second)
	if first != second {
		t.Errorf("milestone count drifted after re-seed: first=%d second=%d", first, second)
	}
	if first < 100 {
		t.Errorf("expected ≥100 milestones after seed, got %d", first)
	}
}

func TestGetPushupEntries_ReflectsSaved(t *testing.T) {
	db := setupPushupDB(t)
	withPushupClock(t, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	router := setupPushupRouter(db, 7)

	patch := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/pushups/entries/2026-07-15", bytes.NewBufferString(`{"count": 99}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(patch, req)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch failed: %d", patch.Code)
	}

	get := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/pushups/entries", nil)
	router.ServeHTTP(get, req2)

	var resp struct {
		Entries []pushupEntryDTO `json:"entries"`
	}
	json.Unmarshal(get.Body.Bytes(), &resp)
	var found *pushupEntryDTO
	for i := range resp.Entries {
		if resp.Entries[i].Date == "2026-07-15" {
			found = &resp.Entries[i]
			break
		}
	}
	if found == nil || found.Count == nil || *found.Count != 99 {
		t.Fatalf("expected 2026-07-15 count=99, got %+v", found)
	}
}
