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

func setupCommentRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Next()
	})
	r.POST("/goals/nodes/:node_id/comments", CreateCommentHandler)
	r.GET("/goals/nodes/:node_id/comments", ListCommentsHandler)
	r.PATCH("/goals/comments/:id", UpdateCommentHandler)
	r.DELETE("/goals/comments/:id", DeleteCommentHandler)
	return r
}

func createTestNode(db *gorm.DB, userID uint) (*data_models.GoalTree, *data_models.GoalNode) {
	tree := data_models.GoalTree{UserID: userID, Name: "Comment Test Tree"}
	db.Create(&tree)
	node := data_models.GoalNode{TreeID: tree.ID, Name: "Test Node", NodeType: "goal", SortOrder: 100}
	db.Create(&node)
	return &tree, &node
}

func TestCreateComment_Success(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	body := `{"body": "This is a comment"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/nodes/%d/comments", node.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp commentResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Body != "This is a comment" {
		t.Errorf("expected body 'This is a comment', got '%s'", resp.Body)
	}
	if resp.NodeID != node.ID {
		t.Errorf("expected node_id %d, got %d", node.ID, resp.NodeID)
	}
	if resp.UserID != 1 {
		t.Errorf("expected user_id 1, got %d", resp.UserID)
	}
}

func TestCreateComment_EmptyBody(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	body := `{"body": ""}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/nodes/%d/comments", node.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateComment_NodeNotFound(t *testing.T) {
	db := setupTestDB(t)
	createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	body := `{"body": "orphan comment"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/goals/nodes/9999/comments", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateComment_OtherUserNode(t *testing.T) {
	db := setupTestDB(t)
	// Node belongs to user 2
	tree := data_models.GoalTree{UserID: 2, Name: "Other User Tree"}
	db.Create(&tree)
	node := data_models.GoalNode{TreeID: tree.ID, Name: "Private", NodeType: "goal", SortOrder: 100}
	db.Create(&node)

	// User 1 tries to comment
	router := setupCommentRouter(db, 1)
	body := `{"body": "sneaky"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/nodes/%d/comments", node.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for other user's node, got %d", w.Code)
	}
}

func TestListComments_Success(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	// Create 3 comments
	for i := 0; i < 3; i++ {
		db.Create(&data_models.GoalComment{NodeID: node.ID, UserID: 1, Body: fmt.Sprintf("Comment %d", i)})
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/nodes/%d/comments", node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []commentResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 3 {
		t.Errorf("expected 3 comments, got %d", len(resp))
	}
}

func TestListComments_ExcludesSoftDeleted(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	// Create 2 comments, soft-delete one
	db.Create(&data_models.GoalComment{NodeID: node.ID, UserID: 1, Body: "visible"})
	now := time.Now()
	db.Create(&data_models.GoalComment{
		NodeID: node.ID, UserID: 1, Body: "deleted",
		Model: gorm.Model{DeletedAt: &now},
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/nodes/%d/comments", node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []commentResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Errorf("expected 1 visible comment, got %d", len(resp))
	}
	if resp[0].Body != "visible" {
		t.Errorf("expected 'visible', got '%s'", resp[0].Body)
	}
}

func TestUpdateComment_Success(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	comment := data_models.GoalComment{NodeID: node.ID, UserID: 1, Body: "original"}
	db.Create(&comment)

	// Small delay so updated_at will differ from created_at
	time.Sleep(10 * time.Millisecond)

	body := `{"body": "edited"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/comments/%d", comment.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commentResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Body != "edited" {
		t.Errorf("expected body 'edited', got '%s'", resp.Body)
	}
	if !resp.UpdatedAt.After(resp.CreatedAt) {
		t.Errorf("expected updated_at (%v) to be after created_at (%v)", resp.UpdatedAt, resp.CreatedAt)
	}
}

func TestUpdateComment_NotFound(t *testing.T) {
	db := setupTestDB(t)
	createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	body := `{"body": "ghost"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/goals/comments/9999", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteComment_Success(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	comment := data_models.GoalComment{NodeID: node.ID, UserID: 1, Body: "to delete"}
	db.Create(&comment)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/comments/%d", comment.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify soft-deleted (not in normal query)
	var found data_models.GoalComment
	err := db.Where("id = ? AND deleted_at IS NULL", comment.ID).First(&found).Error
	if err == nil {
		t.Error("expected comment to be soft-deleted, but it was found")
	}

	// Verify still exists in DB with deleted_at set
	var deleted data_models.GoalComment
	db.Unscoped().Where("id = ?", comment.ID).First(&deleted)
	if deleted.DeletedAt == nil {
		t.Error("expected deleted_at to be set")
	}
}

func TestDeleteComment_SoftDeletedNotInList(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	// Create and then delete a comment
	comment := data_models.GoalComment{NodeID: node.ID, UserID: 1, Body: "will vanish"}
	db.Create(&comment)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/comments/%d", comment.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", w.Code)
	}

	// List should return empty
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", fmt.Sprintf("/goals/nodes/%d/comments", node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}

	var resp []commentResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected 0 comments after delete, got %d", len(resp))
	}
}

func TestUpdateComment_EditConfirmsTimestampDifference(t *testing.T) {
	db := setupTestDB(t)
	_, node := createTestNode(db, 1)
	router := setupCommentRouter(db, 1)

	comment := data_models.GoalComment{NodeID: node.ID, UserID: 1, Body: "initial"}
	db.Create(&comment)

	// Record created_at
	var created data_models.GoalComment
	db.Where("id = ?", comment.ID).First(&created)
	createdAt := created.CreatedAt

	time.Sleep(15 * time.Millisecond)

	body := `{"body": "revised content"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/comments/%d", comment.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commentResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.CreatedAt.Equal(resp.UpdatedAt) {
		t.Errorf("after edit, updated_at (%v) should differ from created_at (%v)", resp.UpdatedAt, resp.CreatedAt)
	}
	if !resp.UpdatedAt.After(createdAt) {
		t.Errorf("updated_at (%v) should be after original created_at (%v)", resp.UpdatedAt, createdAt)
	}
}
