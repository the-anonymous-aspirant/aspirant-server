package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.GoalTree{}, &data_models.GoalNode{}, &data_models.GoalEdge{}, &data_models.GoalComment{})
	t.Cleanup(func() { db.Close() })
	return db
}

func setupTreeRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Next()
	})
	r.POST("/goals/trees", CreateTreeHandler)
	r.GET("/goals/trees", ListTreesHandler)
	r.GET("/goals/trees/:id", GetTreeHandler)
	r.PATCH("/goals/trees/:id", UpdateTreeHandler)
	r.DELETE("/goals/trees/:id", DeleteTreeHandler)
	return r
}

func withUser(req *http.Request, userID uint) *http.Request {
	return req
}

func setupTreeRouterWithUser(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Next()
	})
	r.POST("/goals/trees", CreateTreeHandler)
	r.GET("/goals/trees", ListTreesHandler)
	r.GET("/goals/trees/:id", GetTreeHandler)
	r.PATCH("/goals/trees/:id", UpdateTreeHandler)
	r.DELETE("/goals/trees/:id", DeleteTreeHandler)
	return r
}

func TestCreateTree_Success(t *testing.T) {
	db := setupTestDB(t)
	router := setupTreeRouterWithUser(db, 1)

	body := `{"name": "2026 Goals"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/goals/trees", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp treeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "2026 Goals" {
		t.Errorf("expected name '2026 Goals', got '%s'", resp.Name)
	}
	if resp.RootNodeID != nil {
		t.Errorf("expected root_node_id to be nil, got %v", resp.RootNodeID)
	}
	if resp.ID == 0 {
		t.Error("expected non-zero ID")
	}
}

func TestCreateTree_MissingName(t *testing.T) {
	db := setupTestDB(t)
	router := setupTreeRouterWithUser(db, 1)

	body := `{}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/goals/trees", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListTrees_ReturnsOnlyOwnTrees(t *testing.T) {
	db := setupTestDB(t)

	// Create trees for two users
	db.Create(&data_models.GoalTree{UserID: 1, Name: "User1 Tree"})
	db.Create(&data_models.GoalTree{UserID: 2, Name: "User2 Tree"})
	db.Create(&data_models.GoalTree{UserID: 1, Name: "User1 Other Tree"})

	router := setupTreeRouterWithUser(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/goals/trees", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var trees []treeResponse
	json.Unmarshal(w.Body.Bytes(), &trees)
	if len(trees) != 2 {
		t.Fatalf("expected 2 trees for user 1, got %d", len(trees))
	}
	for _, tree := range trees {
		if tree.Name == "User2 Tree" {
			t.Error("user 1 should not see user 2's tree")
		}
	}
}

func TestGetTree_Returns404ForOtherUser(t *testing.T) {
	db := setupTestDB(t)

	// Create a tree owned by user 2
	tree := data_models.GoalTree{UserID: 2, Name: "User2 Secret"}
	db.Create(&tree)

	// User 1 tries to access it
	router := setupTreeRouterWithUser(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/trees/%d", tree.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when accessing another user's tree, got %d", w.Code)
	}

	var errResp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Error.Code != "not_found" {
		t.Errorf("expected error code 'not_found', got '%s'", errResp.Error.Code)
	}
}

func TestGetTree_Success(t *testing.T) {
	db := setupTestDB(t)

	tree := data_models.GoalTree{UserID: 1, Name: "My Tree"}
	db.Create(&tree)

	router := setupTreeRouterWithUser(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/trees/%d", tree.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp treeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "My Tree" {
		t.Errorf("expected 'My Tree', got '%s'", resp.Name)
	}
}

func TestUpdateTree_Success(t *testing.T) {
	db := setupTestDB(t)

	tree := data_models.GoalTree{UserID: 1, Name: "Old Name"}
	db.Create(&tree)

	router := setupTreeRouterWithUser(db, 1)
	body := `{"name": "New Name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d", tree.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp treeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "New Name" {
		t.Errorf("expected 'New Name', got '%s'", resp.Name)
	}
}

func TestUpdateTree_Returns404ForOtherUser(t *testing.T) {
	db := setupTestDB(t)

	tree := data_models.GoalTree{UserID: 2, Name: "Not Yours"}
	db.Create(&tree)

	router := setupTreeRouterWithUser(db, 1)
	body := `{"name": "Hacked"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d", tree.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteTree_SoftDeletes(t *testing.T) {
	db := setupTestDB(t)

	tree := data_models.GoalTree{UserID: 1, Name: "To Delete"}
	db.Create(&tree)

	router := setupTreeRouterWithUser(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/trees/%d", tree.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify tree is soft-deleted (still in DB but deleted_at set)
	var deleted data_models.GoalTree
	db.Unscoped().Where("id = ?", tree.ID).First(&deleted)
	if deleted.DeletedAt == nil {
		t.Error("expected deleted_at to be set after soft delete")
	}

	// Verify it no longer shows in list
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/goals/trees", nil)
	router.ServeHTTP(w, req)

	var trees []treeResponse
	json.Unmarshal(w.Body.Bytes(), &trees)
	if len(trees) != 0 {
		t.Errorf("expected 0 trees after delete, got %d", len(trees))
	}
}

func TestDeleteTree_Returns404ForOtherUser(t *testing.T) {
	db := setupTestDB(t)

	tree := data_models.GoalTree{UserID: 2, Name: "Not Yours"}
	db.Create(&tree)

	router := setupTreeRouterWithUser(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/trees/%d", tree.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestUserA_CannotSee_UserB_Tree(t *testing.T) {
	db := setupTestDB(t)

	// User B creates trees
	db.Create(&data_models.GoalTree{UserID: 10, Name: "B Private 1"})
	db.Create(&data_models.GoalTree{UserID: 10, Name: "B Private 2"})

	// User A creates a tree
	db.Create(&data_models.GoalTree{UserID: 5, Name: "A's Tree"})

	// User A lists — should only see their own
	routerA := setupTreeRouterWithUser(db, 5)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/goals/trees", nil)
	routerA.ServeHTTP(w, req)

	var trees []treeResponse
	json.Unmarshal(w.Body.Bytes(), &trees)
	if len(trees) != 1 {
		t.Fatalf("user A expected 1 tree, got %d", len(trees))
	}
	if trees[0].Name != "A's Tree" {
		t.Errorf("expected 'A's Tree', got '%s'", trees[0].Name)
	}

	// User A tries to get B's tree by ID — 404
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/goals/trees/1", nil)
	routerA.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("user A accessing B's tree: expected 404, got %d", w.Code)
	}

	// User B can see their own trees
	routerB := setupTreeRouterWithUser(db, 10)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/goals/trees", nil)
	routerB.ServeHTTP(w, req)

	json.Unmarshal(w.Body.Bytes(), &trees)
	if len(trees) != 2 {
		t.Errorf("user B expected 2 trees, got %d", len(trees))
	}
}
