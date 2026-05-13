package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func setupSessionRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Next()
	})
	r.POST("/goals/trees/:id/open", OpenTreeForEditingHandler)
	r.POST("/goals/trees/:id/take-over", TakeOverTreeEditingHandler)
	r.POST("/goals/trees/:id/release", ReleaseTreeEditingHandler)
	r.PATCH("/goals/trees/:id", UpdateTreeHandler)
	return r
}

func setupSessionNodeRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Next()
	})
	r.POST("/goals/trees/:id/nodes", CreateNodeHandler)
	r.PATCH("/goals/trees/:id/nodes/:node_id", UpdateNodeHandler)
	return r
}

func TestOpenTree_AcquiresLock(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Test Tree"}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp openTreeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "acquired" {
		t.Errorf("expected status 'acquired', got '%s'", resp.Status)
	}
	if resp.EditingSessionID == "" {
		t.Error("expected non-empty editing_session_id")
	}
	if resp.TreeID != tree.ID {
		t.Errorf("expected tree_id %d, got %d", tree.ID, resp.TreeID)
	}
}

func TestOpenTree_SecondOpen_Returns409(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Test Tree"}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)

	// First open — acquires lock
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("first open: expected 200, got %d", w1.Code)
	}

	// Second open — should get 409 "locked_elsewhere"
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("second open: expected 409, got %d: %s", w2.Code, w2.Body.String())
	}

	var resp lockedElsewhereResponse
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp.Status != "locked_elsewhere" {
		t.Errorf("expected status 'locked_elsewhere', got '%s'", resp.Status)
	}
	if !resp.CanTakeOver {
		t.Error("expected can_take_over to be true")
	}
	if resp.ExistingSessionID == "" {
		t.Error("expected existing_session_id to be populated")
	}
}

func TestTakeOver_InvalidatesPriorSession(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Test Tree"}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)

	// First open — get session A
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w1, req1)

	var sessionA openTreeResponse
	json.Unmarshal(w1.Body.Bytes(), &sessionA)

	// Take-over — get session B
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/take-over", tree.ID), nil)
	router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("take-over: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var sessionB openTreeResponse
	json.Unmarshal(w2.Body.Bytes(), &sessionB)
	if sessionB.Status != "taken_over" {
		t.Errorf("expected status 'taken_over', got '%s'", sessionB.Status)
	}
	if sessionB.EditingSessionID == sessionA.EditingSessionID {
		t.Error("take-over should generate a new session ID")
	}

	// Session A's PATCH should now return 409
	body := `{"name": "Attempted Update"}`
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d", tree.ID), bytes.NewBufferString(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Editing-Session-ID", sessionA.EditingSessionID)
	router.ServeHTTP(w3, req3)

	if w3.Code != http.StatusConflict {
		t.Fatalf("old session PATCH: expected 409, got %d: %s", w3.Code, w3.Body.String())
	}

	var errResp ErrorResponse
	json.Unmarshal(w3.Body.Bytes(), &errResp)
	if errResp.Error.Code != "session_superseded" {
		t.Errorf("expected error code 'session_superseded', got '%s'", errResp.Error.Code)
	}

	// Session B's PATCH should succeed
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d", tree.ID), bytes.NewBufferString(body))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("X-Editing-Session-ID", sessionB.EditingSessionID)
	router.ServeHTTP(w4, req4)

	if w4.Code != http.StatusOK {
		t.Fatalf("new session PATCH: expected 200, got %d: %s", w4.Code, w4.Body.String())
	}
}

func TestSessionExpiry_AllowsNewOpen(t *testing.T) {
	db := setupTestDB(t)

	// Create tree with an expired session
	expiredTime := time.Now().Add(-31 * time.Minute)
	sessionID := "old-session-id"
	tree := data_models.GoalTree{
		UserID:                     1,
		Name:                       "Expired Lock Tree",
		EditingSessionID:           &sessionID,
		EditingSessionLastActivity: &expiredTime,
	}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)

	// Open should succeed because the lock is expired
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("open expired tree: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp openTreeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "acquired" {
		t.Errorf("expected status 'acquired', got '%s'", resp.Status)
	}
}

func TestWriteWithoutSession_SucceedsWhenNoLock(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Unlocked Tree"}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)

	// PATCH without any session header should work when tree has no lock
	body := `{"name": "Updated Name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d", tree.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PATCH unlocked tree: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWriteWithWrongSession_Returns409(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Locked Tree"}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)

	// Acquire lock
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w1, req1)

	// PATCH with wrong session ID → 409
	body := `{"name": "Bad Update"}`
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d", tree.ID), bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Editing-Session-ID", "wrong-session-id")
	router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("PATCH with wrong session: expected 409, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestNodeWrite_RequiresValidSession(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Test Tree"}
	db.Create(&tree)

	// Create a node
	node := data_models.GoalNode{TreeID: tree.ID, Name: "Root", NodeType: "goal", SortOrder: 100}
	db.Create(&node)

	// Use session router to acquire the lock
	sessionRouter := setupSessionRouter(db, 1)
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	sessionRouter.ServeHTTP(w1, req1)

	var session openTreeResponse
	json.Unmarshal(w1.Body.Bytes(), &session)

	// Use node router for node operations
	nodeRouter := setupSessionNodeRouter(db, 1)

	// Update node with wrong session → 409
	body := `{"name": "Updated Node"}`
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, node.ID), bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Editing-Session-ID", "bogus")
	nodeRouter.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("node PATCH with wrong session: expected 409, got %d: %s", w2.Code, w2.Body.String())
	}

	// Update node with correct session → 200
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, node.ID), bytes.NewBufferString(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Editing-Session-ID", session.EditingSessionID)
	nodeRouter.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("node PATCH with correct session: expected 200, got %d: %s", w3.Code, w3.Body.String())
	}
}

func TestReleaseSession(t *testing.T) {
	db := setupTestDB(t)
	tree := data_models.GoalTree{UserID: 1, Name: "Release Test"}
	db.Create(&tree)

	router := setupSessionRouter(db, 1)

	// Acquire lock
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w1, req1)

	var session openTreeResponse
	json.Unmarshal(w1.Body.Bytes(), &session)

	// Release with correct session
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/release", tree.ID), nil)
	req2.Header.Set("X-Editing-Session-ID", session.EditingSessionID)
	router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusNoContent {
		t.Fatalf("release: expected 204, got %d: %s", w2.Code, w2.Body.String())
	}

	// Now open should succeed (no lock)
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/open", tree.ID), nil)
	router.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("re-open after release: expected 200, got %d: %s", w3.Code, w3.Body.String())
	}
}
