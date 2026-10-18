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

func setupSitupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.SitupEntry{})
	t.Cleanup(func() { db.Close() })
	return db
}

func setupSitupRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		if userID > 0 {
			c.Set("user_id", userID)
		}
		c.Next()
	})
	r.GET("/situps/entries", GetSitupEntriesHandler)
	r.PATCH("/situps/entries/:date", PatchSitupEntryHandler)
	return r
}

// withSitupClock temporarily swaps the package clock for tests.
func withSitupClock(t *testing.T, fixed time.Time) {
	t.Helper()
	original := situpNow
	situpNow = func() time.Time { return fixed }
	t.Cleanup(func() { situpNow = original })
}

func patchSitup(router *gin.Engine, date, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/situps/entries/"+date, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func getSitupEntries(t *testing.T, router *gin.Engine) []situpEntryDTO {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/situps/entries", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Entries []situpEntryDTO `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	return resp.Entries
}

func TestGetSitupEntries_GapFills57Days(t *testing.T) {
	db := setupSitupDB(t)
	entries := getSitupEntries(t, setupSitupRouter(db, 1))

	// 2026-10-18..2026-12-13 inclusive: 14 (Oct) + 30 (Nov) + 13 (Dec).
	if len(entries) != 57 {
		t.Fatalf("expected 57 gap-filled entries, got %d", len(entries))
	}
	if entries[0].Date != "2026-10-18" {
		t.Errorf("expected first date 2026-10-18, got %s", entries[0].Date)
	}
	if entries[56].Date != "2026-12-13" {
		t.Errorf("expected last date 2026-12-13, got %s", entries[56].Date)
	}
	for i, e := range entries {
		if e.Count != nil {
			t.Errorf("entry %d: expected nil count on empty db, got %v", i, *e.Count)
		}
	}
}

func TestPatchSitupEntry_WithinEditWindowPersists(t *testing.T) {
	db := setupSitupDB(t)
	withSitupClock(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	router := setupSitupRouter(db, 42)

	if w := patchSitup(router, "2026-11-05", `{"count": 60}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	saved, err := data_models.GetSitupEntryByDate(db, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("error reading back: %v", err)
	}
	if saved == nil || saved.Count == nil || *saved.Count != 60 {
		t.Fatalf("expected saved count 60, got %+v", saved)
	}
	if saved.UpdatedBy != 42 {
		t.Errorf("expected UpdatedBy=42, got %d", saved.UpdatedBy)
	}

	// The GET surfaces the saved row in its slot: 2026-11-05 is day 19 (index 18).
	entries := getSitupEntries(t, router)
	if entries[18].Date != "2026-11-05" || entries[18].Count == nil || *entries[18].Count != 60 {
		t.Errorf("expected entry 18 = 2026-11-05 count 60, got %+v", entries[18])
	}
}

func TestPatchSitupEntry_RepatchOverwritesOneRow(t *testing.T) {
	db := setupSitupDB(t)
	withSitupClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	router := setupSitupRouter(db, 1)

	for _, body := range []string{`{"count": 30}`, `{"count": 55}`} {
		if w := patchSitup(router, "2026-10-20", body); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}
	var n int
	db.Model(&data_models.SitupEntry{}).Count(&n)
	if n != 1 {
		t.Fatalf("expected one row after re-PATCH, got %d", n)
	}
	saved, _ := data_models.GetSitupEntryByDate(db, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	if saved == nil || saved.Count == nil || *saved.Count != 55 {
		t.Fatalf("expected overwritten count 55, got %+v", saved)
	}
}

func TestPatchSitupEntry_EditWindowIsTodayPlusMinusTwo(t *testing.T) {
	db := setupSitupDB(t)
	// today=2026-11-20 → editable: 18, 19, 20, 21, 22
	withSitupClock(t, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	router := setupSitupRouter(db, 1)

	for _, d := range []string{"2026-11-18", "2026-11-19", "2026-11-20", "2026-11-21", "2026-11-22"} {
		if w := patchSitup(router, d, `{"count": 10}`); w.Code != http.StatusOK {
			t.Errorf("date %s expected 200, got %d: %s", d, w.Code, w.Body.String())
		}
	}
	for _, d := range []string{"2026-11-17", "2026-11-23", "2026-12-01"} {
		if w := patchSitup(router, d, `{"count": 10}`); w.Code != http.StatusForbidden {
			t.Errorf("date %s expected 403, got %d: %s", d, w.Code, w.Body.String())
		}
	}
}

func TestPatchSitupEntry_OutsideChallengeWindowReturns400(t *testing.T) {
	cases := []struct {
		now  time.Time
		date string
	}{
		{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), "2026-10-17"}, // day before start
		{time.Date(2026, 12, 14, 12, 0, 0, 0, time.UTC), "2026-12-14"}, // day after end
	}
	for _, tc := range cases {
		db := setupSitupDB(t)
		withSitupClock(t, tc.now)
		router := setupSitupRouter(db, 1)
		if w := patchSitup(router, tc.date, `{"count": 10}`); w.Code != http.StatusBadRequest {
			t.Errorf("date %s expected 400, got %d: %s", tc.date, w.Code, w.Body.String())
		}
	}
}

func TestPatchSitupEntry_WindowEdgesAccepted(t *testing.T) {
	for _, d := range []time.Time{
		time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 13, 12, 0, 0, 0, time.UTC),
	} {
		db := setupSitupDB(t)
		withSitupClock(t, d)
		router := setupSitupRouter(db, 1)
		if w := patchSitup(router, d.Format("2006-01-02"), `{"count": 60}`); w.Code != http.StatusOK {
			t.Errorf("edge %s expected 200, got %d: %s", d.Format("2006-01-02"), w.Code, w.Body.String())
		}
	}
}

func TestPatchSitupEntry_NegativeCountReturns400(t *testing.T) {
	db := setupSitupDB(t)
	withSitupClock(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	router := setupSitupRouter(db, 1)

	if w := patchSitup(router, "2026-11-05", `{"count": -3}`); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for negative count, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchSitupEntry_BadDateFormatReturns400(t *testing.T) {
	db := setupSitupDB(t)
	router := setupSitupRouter(db, 1)

	if w := patchSitup(router, "not-a-date", `{"count": 1}`); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad date, got %d: %s", w.Code, w.Body.String())
	}
}
