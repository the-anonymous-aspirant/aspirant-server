package handlers

import (
	"bytes"
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

func setupScratchpadDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.Scratchpad{})
	t.Cleanup(func() { db.Close() })
	return db
}

func setupScratchpadRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/users/me/scratchpad", GetScratchpadHandler)
	r.PUT("/users/me/scratchpad", PutScratchpadHandler)
	return r
}

func putScratchpad(t *testing.T, router *gin.Engine, text string) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(putScratchpadRequest{Text: text})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/users/me/scratchpad", bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func getScratchpad(t *testing.T, router *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/users/me/scratchpad", nil)
	router.ServeHTTP(w, req)
	return w
}

// A user who has never written gets an empty scratchpad, not a 404.
func TestGetScratchpad_EmptyForNewUser(t *testing.T) {
	db := setupScratchpadDB(t)
	router := setupScratchpadRouter(db, 1)

	w := getScratchpad(t, router)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp scratchpadResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Text != "" {
		t.Errorf("expected empty text, got %q", resp.Text)
	}
	if resp.UpdatedAt != nil {
		t.Errorf("expected nil updated_at for never-written scratchpad, got %v", resp.UpdatedAt)
	}
}

func TestPutScratchpad_PersistsAndReadsBack(t *testing.T) {
	db := setupScratchpadDB(t)
	router := setupScratchpadRouter(db, 1)

	w := putScratchpad(t, router, "hello world")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on PUT, got %d: %s", w.Code, w.Body.String())
	}
	var putResp scratchpadResponse
	json.Unmarshal(w.Body.Bytes(), &putResp)
	if putResp.Text != "hello world" {
		t.Errorf("expected PUT echo 'hello world', got %q", putResp.Text)
	}
	if putResp.UpdatedAt == nil {
		t.Error("expected non-nil updated_at after PUT")
	}

	g := getScratchpad(t, router)
	var getResp scratchpadResponse
	json.Unmarshal(g.Body.Bytes(), &getResp)
	if getResp.Text != "hello world" {
		t.Errorf("expected GET to read back 'hello world', got %q", getResp.Text)
	}
}

func TestPutScratchpad_Overwrites(t *testing.T) {
	db := setupScratchpadDB(t)
	router := setupScratchpadRouter(db, 1)

	putScratchpad(t, router, "first")
	putScratchpad(t, router, "second")

	g := getScratchpad(t, router)
	var resp scratchpadResponse
	json.Unmarshal(g.Body.Bytes(), &resp)
	if resp.Text != "second" {
		t.Errorf("expected overwrite to 'second', got %q", resp.Text)
	}

	// Exactly one row for the user — overwrite, not append.
	var count int
	db.Model(&data_models.Scratchpad{}).Where("user_id = ?", 1).Count(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 scratchpad row, got %d", count)
	}
}

// The core invariant: one user's scratchpad is never visible to another.
func TestScratchpad_PerUserIsolation(t *testing.T) {
	db := setupScratchpadDB(t)

	routerA := setupScratchpadRouter(db, 1)
	routerB := setupScratchpadRouter(db, 2)

	putScratchpad(t, routerA, "user A private note")

	// User B has never written — must see an empty scratchpad, not A's.
	wb := getScratchpad(t, routerB)
	var respB scratchpadResponse
	json.Unmarshal(wb.Body.Bytes(), &respB)
	if respB.Text != "" {
		t.Fatalf("cross-user leak: user B saw %q", respB.Text)
	}

	// User B writes their own — must not disturb A.
	putScratchpad(t, routerB, "user B note")

	wa := getScratchpad(t, routerA)
	var respA scratchpadResponse
	json.Unmarshal(wa.Body.Bytes(), &respA)
	if respA.Text != "user A private note" {
		t.Errorf("user A's scratchpad was disturbed: got %q", respA.Text)
	}
}

func TestPutScratchpad_RejectsOversize(t *testing.T) {
	db := setupScratchpadDB(t)
	router := setupScratchpadRouter(db, 1)

	huge := strings.Repeat("x", (1<<20)+1) // 1 MiB + 1 byte
	w := putScratchpad(t, router, huge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversize body, got %d: %s", w.Code, w.Body.String())
	}

	// Nothing persisted.
	var count int
	db.Model(&data_models.Scratchpad{}).Where("user_id = ?", 1).Count(&count)
	if count != 0 {
		t.Errorf("expected no row persisted on oversize reject, got %d", count)
	}
}

// A body at the ceiling is accepted.
func TestPutScratchpad_AllowsAtCeiling(t *testing.T) {
	db := setupScratchpadDB(t)
	router := setupScratchpadRouter(db, 1)

	atLimit := strings.Repeat("y", 1<<20) // exactly 1 MiB
	w := putScratchpad(t, router, atLimit)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for at-ceiling body, got %d: %s", w.Code, w.Body.String())
	}
}
