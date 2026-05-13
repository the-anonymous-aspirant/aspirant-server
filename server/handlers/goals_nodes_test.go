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

func setupNodeRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Next()
	})
	r.POST("/goals/trees/:id/nodes", CreateNodeHandler)
	r.GET("/goals/trees/:id/nodes", ListNodesHandler)
	r.GET("/goals/trees/:id/nodes/:node_id", GetNodeHandler)
	r.PATCH("/goals/trees/:id/nodes/:node_id", UpdateNodeHandler)
	r.DELETE("/goals/trees/:id/nodes/:node_id", DeleteNodeHandler)
	r.POST("/goals/trees/:id/nodes/:node_id/complete", CompleteNodeHandler)
	r.POST("/goals/trees/:id/nodes/:node_id/uncomplete", UncompleteNodeHandler)
	return r
}

func createTestTree(db *gorm.DB, userID uint) *data_models.GoalTree {
	tree := data_models.GoalTree{UserID: userID, Name: "Test Tree"}
	db.Create(&tree)
	return &tree
}

func TestCreateNode_Success(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	body := fmt.Sprintf(`{"name": "Root Goal", "node_type": "goal"}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes", tree.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "Root Goal" {
		t.Errorf("expected name 'Root Goal', got '%s'", resp.Name)
	}
	if resp.TreeID != tree.ID {
		t.Errorf("expected tree_id %d, got %d", tree.ID, resp.TreeID)
	}
	if resp.SortOrder != 100 {
		t.Errorf("expected sort_order 100, got %d", resp.SortOrder)
	}
	if resp.ParentID != nil {
		t.Errorf("expected nil parent_id, got %v", resp.ParentID)
	}

	// Verify tree.root_node_id was set
	var updatedTree data_models.GoalTree
	db.First(&updatedTree, tree.ID)
	if updatedTree.RootNodeID == nil || *updatedTree.RootNodeID != resp.ID {
		t.Errorf("expected tree root_node_id to be %d", resp.ID)
	}
}

func TestCreateNode_WithParent(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	parent := data_models.GoalNode{TreeID: tree.ID, Name: "Parent", NodeType: "goal", SortOrder: 100}
	db.Create(&parent)

	router := setupNodeRouter(db, 1)
	body := fmt.Sprintf(`{"name": "Child", "node_type": "milestone", "parent_id": %d}`, parent.ID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes", tree.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ParentID == nil || *resp.ParentID != parent.ID {
		t.Errorf("expected parent_id %d, got %v", parent.ID, resp.ParentID)
	}

	// Verify edge was created
	var edge data_models.GoalEdge
	if err := db.Where("from_id = ? AND to_id = ?", parent.ID, resp.ID).First(&edge).Error; err != nil {
		t.Errorf("expected edge from parent to child, got error: %v", err)
	}
}

func TestCreateNode_MaxDepthExceeded(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	// Build chain: depth 0 → 1 → 2 → 3 → 4 (5 nodes, max depth reached)
	var lastID uint
	for i := 0; i < 5; i++ {
		node := data_models.GoalNode{TreeID: tree.ID, Name: fmt.Sprintf("Level %d", i), NodeType: "step", SortOrder: 100}
		if i > 0 {
			node.ParentID = &lastID
		}
		db.Create(&node)
		lastID = node.ID
	}

	router := setupNodeRouter(db, 1)
	body := fmt.Sprintf(`{"name": "Too Deep", "node_type": "step", "parent_id": %d}`, lastID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes", tree.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}

	var errResp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Error.Code != "max_depth_exceeded" {
		t.Errorf("expected error code 'max_depth_exceeded', got '%s'", errResp.Error.Code)
	}
}

func TestCreateNode_SortOrderAutoIncrement(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Create three root siblings
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"name": "Node %d", "node_type": "goal"}`, i)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes", tree.ID), bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("node %d: expected 201, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	// List and verify sort_orders are 100, 200, 300
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/trees/%d/nodes", tree.ID), nil)
	router.ServeHTTP(w, req)

	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)
	expected := []int{100, 200, 300}
	for i, n := range nodes {
		if n.SortOrder != expected[i] {
			t.Errorf("node %d: expected sort_order %d, got %d", i, expected[i], n.SortOrder)
		}
	}
}

func TestDeleteNode_EdgeSurvival_ABC(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	// Build A → B → C
	nodeA := data_models.GoalNode{TreeID: tree.ID, Name: "A", NodeType: "goal", SortOrder: 100}
	db.Create(&nodeA)

	nodeB := data_models.GoalNode{TreeID: tree.ID, Name: "B", NodeType: "milestone", ParentID: &nodeA.ID, SortOrder: 100}
	db.Create(&nodeB)

	nodeC := data_models.GoalNode{TreeID: tree.ID, Name: "C", NodeType: "step", ParentID: &nodeB.ID, SortOrder: 100}
	db.Create(&nodeC)

	// Create edges A→B and B→C
	db.Create(&data_models.GoalEdge{TreeID: tree.ID, FromID: nodeA.ID, ToID: nodeB.ID})
	db.Create(&data_models.GoalEdge{TreeID: tree.ID, FromID: nodeB.ID, ToID: nodeC.ID})

	router := setupNodeRouter(db, 1)

	// Delete B
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, nodeB.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify B is soft-deleted
	var deletedB data_models.GoalNode
	db.Unscoped().Where("id = ?", nodeB.ID).First(&deletedB)
	if deletedB.DeletedAt == nil {
		t.Error("expected B to be soft-deleted")
	}

	// Verify C's parent_id is now A
	var updatedC data_models.GoalNode
	db.Where("id = ?", nodeC.ID).First(&updatedC)
	if updatedC.ParentID == nil || *updatedC.ParentID != nodeA.ID {
		t.Errorf("expected C's parent to be A (id=%d), got %v", nodeA.ID, updatedC.ParentID)
	}

	// Verify edge A→C exists
	var edgeAC data_models.GoalEdge
	if err := db.Where("from_id = ? AND to_id = ?", nodeA.ID, nodeC.ID).First(&edgeAC).Error; err != nil {
		t.Errorf("expected edge A→C to exist after deleting B, got error: %v", err)
	}

	// Verify old edges A→B and B→C are gone
	var count int
	db.Model(&data_models.GoalEdge{}).Where("from_id = ? OR to_id = ?", nodeB.ID, nodeB.ID).Count(&count)
	if count != 0 {
		t.Errorf("expected no edges referencing B, got %d", count)
	}
}

func TestDeleteNode_RootWithChildren(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	// Root node with one child
	root := data_models.GoalNode{TreeID: tree.ID, Name: "Root", NodeType: "goal", SortOrder: 100}
	db.Create(&root)
	tree.RootNodeID = &root.ID
	db.Save(tree)

	child := data_models.GoalNode{TreeID: tree.ID, Name: "Child", NodeType: "milestone", ParentID: &root.ID, SortOrder: 100}
	db.Create(&child)
	db.Create(&data_models.GoalEdge{TreeID: tree.ID, FromID: root.ID, ToID: child.ID})

	router := setupNodeRouter(db, 1)

	// Delete root — child becomes new root
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, root.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify child is now the tree root
	var updatedTree data_models.GoalTree
	db.First(&updatedTree, tree.ID)
	if updatedTree.RootNodeID == nil || *updatedTree.RootNodeID != child.ID {
		t.Errorf("expected tree root to be child (id=%d), got %v", child.ID, updatedTree.RootNodeID)
	}

	// Verify child's parent_id is nil (no parent)
	var updatedChild data_models.GoalNode
	db.Where("id = ?", child.ID).First(&updatedChild)
	if updatedChild.ParentID != nil {
		t.Errorf("expected child's parent to be nil after root deletion, got %v", updatedChild.ParentID)
	}
}

func TestCompleteNode_Success(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	node := data_models.GoalNode{TreeID: tree.ID, Name: "Test", NodeType: "step", SortOrder: 100}
	db.Create(&node)

	router := setupNodeRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}
}

func TestCompleteNode_AlreadyCompleted(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	node := data_models.GoalNode{TreeID: tree.ID, Name: "Done", NodeType: "step", SortOrder: 100}
	db.Create(&node)
	// Mark completed directly
	db.Exec("UPDATE goal_nodes SET completed_at = datetime('now') WHERE id = ?", node.ID)

	router := setupNodeRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUncompleteNode_Success(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	node := data_models.GoalNode{TreeID: tree.ID, Name: "Done", NodeType: "step", SortOrder: 100}
	db.Create(&node)
	db.Exec("UPDATE goal_nodes SET completed_at = datetime('now') WHERE id = ?", node.ID)

	router := setupNodeRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/uncomplete", tree.ID, node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.CompletedAt != nil {
		t.Error("expected completed_at to be nil after uncomplete")
	}
}

func TestUncompleteNode_NotCompleted(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	node := data_models.GoalNode{TreeID: tree.ID, Name: "Active", NodeType: "step", SortOrder: 100}
	db.Create(&node)

	router := setupNodeRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/uncomplete", tree.ID, node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetNode_OtherUserTree(t *testing.T) {
	db := setupTestDB(t)

	// Tree owned by user 2
	tree := data_models.GoalTree{UserID: 2, Name: "Private"}
	db.Create(&tree)
	node := data_models.GoalNode{TreeID: tree.ID, Name: "Secret", NodeType: "goal", SortOrder: 100}
	db.Create(&node)

	// User 1 tries to access
	router := setupNodeRouter(db, 1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, node.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for other user's tree, got %d", w.Code)
	}
}

func TestUpdateNode_PartialUpdate(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	node := data_models.GoalNode{TreeID: tree.ID, Name: "Original", NodeType: "goal", Color: "blue", Body: "desc", SortOrder: 100}
	db.Create(&node)

	router := setupNodeRouter(db, 1)
	body := `{"name": "Updated"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, node.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "Updated" {
		t.Errorf("expected name 'Updated', got '%s'", resp.Name)
	}
	if resp.Color != "blue" {
		t.Errorf("expected color 'blue' to be preserved, got '%s'", resp.Color)
	}
	if resp.Body != "desc" {
		t.Errorf("expected body 'desc' to be preserved, got '%s'", resp.Body)
	}
}

// --- Auto-completion rollup tests ---

func TestAutoComplete_AllChildrenCompleted_ParentAutoCompletes(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Build: Root → [Child1, Child2, Child3]
	root := data_models.GoalNode{TreeID: tree.ID, Name: "Root", NodeType: "goal", SortOrder: 100}
	db.Create(&root)
	child1 := data_models.GoalNode{TreeID: tree.ID, Name: "C1", NodeType: "step", ParentID: &root.ID, SortOrder: 100}
	db.Create(&child1)
	child2 := data_models.GoalNode{TreeID: tree.ID, Name: "C2", NodeType: "step", ParentID: &root.ID, SortOrder: 200}
	db.Create(&child2)
	child3 := data_models.GoalNode{TreeID: tree.ID, Name: "C3", NodeType: "step", ParentID: &root.ID, SortOrder: 300}
	db.Create(&child3)

	// Complete child1 and child2 — root should NOT auto-complete yet
	for _, id := range []uint{child1.ID, child2.ID} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, id), nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("complete child %d: expected 200, got %d: %s", id, w.Code, w.Body.String())
		}
	}

	var rootCheck data_models.GoalNode
	db.Where("id = ?", root.ID).First(&rootCheck)
	if rootCheck.CompletedAt != nil {
		t.Fatal("root should NOT be auto-completed yet (child3 still open)")
	}

	// Complete child3 — root should now auto-complete
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, child3.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("complete child3: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	db.Where("id = ?", root.ID).First(&rootCheck)
	if rootCheck.CompletedAt == nil {
		t.Fatal("root should be auto-completed after all children completed")
	}
	if rootCheck.ManualComplete {
		t.Error("root should NOT have manual_complete flag (was auto-completed)")
	}
}

func TestAutoComplete_CascadesUpMultipleLevels(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Build: Grandparent → Parent → Leaf
	gp := data_models.GoalNode{TreeID: tree.ID, Name: "GP", NodeType: "goal", SortOrder: 100}
	db.Create(&gp)
	parent := data_models.GoalNode{TreeID: tree.ID, Name: "Parent", NodeType: "milestone", ParentID: &gp.ID, SortOrder: 100}
	db.Create(&parent)
	leaf := data_models.GoalNode{TreeID: tree.ID, Name: "Leaf", NodeType: "step", ParentID: &parent.ID, SortOrder: 100}
	db.Create(&leaf)

	// Complete the leaf — should cascade through parent and grandparent
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, leaf.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var parentCheck data_models.GoalNode
	db.Where("id = ?", parent.ID).First(&parentCheck)
	if parentCheck.CompletedAt == nil {
		t.Error("parent should be auto-completed")
	}

	var gpCheck data_models.GoalNode
	db.Where("id = ?", gp.ID).First(&gpCheck)
	if gpCheck.CompletedAt == nil {
		t.Error("grandparent should be auto-completed")
	}
}

func TestAutoComplete_ManualOverride_BlocksRollup(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Build: GP → Parent(manual_complete=true) → Leaf
	gp := data_models.GoalNode{TreeID: tree.ID, Name: "GP", NodeType: "goal", SortOrder: 100}
	db.Create(&gp)
	parent := data_models.GoalNode{TreeID: tree.ID, Name: "Parent", NodeType: "milestone", ParentID: &gp.ID, SortOrder: 100, ManualComplete: true}
	db.Create(&parent)
	leaf := data_models.GoalNode{TreeID: tree.ID, Name: "Leaf", NodeType: "step", ParentID: &parent.ID, SortOrder: 100}
	db.Create(&leaf)

	// Complete the leaf — parent has manual_complete so rollup stops there
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, leaf.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var parentCheck data_models.GoalNode
	db.Where("id = ?", parent.ID).First(&parentCheck)
	if parentCheck.CompletedAt != nil {
		t.Error("parent should NOT auto-complete when manual_complete is set")
	}

	var gpCheck data_models.GoalNode
	db.Where("id = ?", gp.ID).First(&gpCheck)
	if gpCheck.CompletedAt != nil {
		t.Error("grandparent should NOT auto-complete (blocked at parent)")
	}
}

func TestAutoComplete_ManualCompleteOnNode(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Build: Parent → [Child1(incomplete), Child2]
	parent := data_models.GoalNode{TreeID: tree.ID, Name: "Parent", NodeType: "goal", SortOrder: 100}
	db.Create(&parent)
	child1 := data_models.GoalNode{TreeID: tree.ID, Name: "C1", NodeType: "step", ParentID: &parent.ID, SortOrder: 100}
	db.Create(&child1)
	child2 := data_models.GoalNode{TreeID: tree.ID, Name: "C2", NodeType: "step", ParentID: &parent.ID, SortOrder: 200}
	db.Create(&child2)

	// Complete parent with manual_complete flag (bypasses children check)
	body := `{"manual_complete": true}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, parent.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.CompletedAt == nil {
		t.Error("node should be completed")
	}
	if !resp.ManualComplete {
		t.Error("node should have manual_complete=true")
	}
}

func TestUncomplete_PropagatesUpward(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Build: GP → Parent → [Leaf1, Leaf2]
	gp := data_models.GoalNode{TreeID: tree.ID, Name: "GP", NodeType: "goal", SortOrder: 100}
	db.Create(&gp)
	parent := data_models.GoalNode{TreeID: tree.ID, Name: "Parent", NodeType: "milestone", ParentID: &gp.ID, SortOrder: 100}
	db.Create(&parent)
	leaf1 := data_models.GoalNode{TreeID: tree.ID, Name: "L1", NodeType: "step", ParentID: &parent.ID, SortOrder: 100}
	db.Create(&leaf1)
	leaf2 := data_models.GoalNode{TreeID: tree.ID, Name: "L2", NodeType: "step", ParentID: &parent.ID, SortOrder: 200}
	db.Create(&leaf2)

	// Complete both leaves — parent and GP auto-complete
	for _, id := range []uint{leaf1.ID, leaf2.ID} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, id), nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("complete %d: expected 200, got %d", id, w.Code)
		}
	}

	// Verify GP is auto-completed
	var gpCheck data_models.GoalNode
	db.Where("id = ?", gp.ID).First(&gpCheck)
	if gpCheck.CompletedAt == nil {
		t.Fatal("GP should be auto-completed after all leaves done")
	}

	// Uncomplete leaf1 — should propagate up through parent and GP
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/uncomplete", tree.ID, leaf1.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("uncomplete leaf1: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var parentCheck data_models.GoalNode
	db.Where("id = ?", parent.ID).First(&parentCheck)
	if parentCheck.CompletedAt != nil {
		t.Error("parent should be uncompleted after child uncompleted")
	}

	db.Where("id = ?", gp.ID).First(&gpCheck)
	if gpCheck.CompletedAt != nil {
		t.Error("GP should be uncompleted after descendant uncompleted")
	}
}

func TestUncomplete_StopsAtManualComplete(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Build: GP(manual_complete+completed) → Parent → Leaf
	gp := data_models.GoalNode{TreeID: tree.ID, Name: "GP", NodeType: "goal", SortOrder: 100, ManualComplete: true}
	db.Create(&gp)
	db.Exec("UPDATE goal_nodes SET completed_at = datetime('now') WHERE id = ?", gp.ID)

	parent := data_models.GoalNode{TreeID: tree.ID, Name: "Parent", NodeType: "milestone", ParentID: &gp.ID, SortOrder: 100}
	db.Create(&parent)
	leaf := data_models.GoalNode{TreeID: tree.ID, Name: "Leaf", NodeType: "step", ParentID: &parent.ID, SortOrder: 100}
	db.Create(&leaf)

	// Complete leaf and parent so they're all done
	for _, id := range []uint{leaf.ID, parent.ID} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, id), nil)
		router.ServeHTTP(w, req)
	}

	// Uncomplete leaf — parent should uncomplete, but GP stays (manual_complete)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/uncomplete", tree.ID, leaf.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("uncomplete leaf: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var parentCheck data_models.GoalNode
	db.Where("id = ?", parent.ID).First(&parentCheck)
	if parentCheck.CompletedAt != nil {
		t.Error("parent should be uncompleted")
	}

	var gpCheck data_models.GoalNode
	db.Where("id = ?", gp.ID).First(&gpCheck)
	if gpCheck.CompletedAt == nil {
		t.Error("GP should stay completed (manual_complete=true blocks propagation)")
	}
}

func TestAutoComplete_LeafNodeNoChildren(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// A single root node with no children — completing it should just work
	root := data_models.GoalNode{TreeID: tree.ID, Name: "Root", NodeType: "goal", SortOrder: 100}
	db.Create(&root)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", tree.ID, root.ID), nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp nodeResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.CompletedAt == nil {
		t.Error("root should be completed")
	}
}

func TestDeleteNode_MultipleChildren_EdgeSurvival(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)

	// A → B → C1, C2
	nodeA := data_models.GoalNode{TreeID: tree.ID, Name: "A", NodeType: "goal", SortOrder: 100}
	db.Create(&nodeA)
	nodeB := data_models.GoalNode{TreeID: tree.ID, Name: "B", NodeType: "milestone", ParentID: &nodeA.ID, SortOrder: 100}
	db.Create(&nodeB)
	nodeC1 := data_models.GoalNode{TreeID: tree.ID, Name: "C1", NodeType: "step", ParentID: &nodeB.ID, SortOrder: 100}
	db.Create(&nodeC1)
	nodeC2 := data_models.GoalNode{TreeID: tree.ID, Name: "C2", NodeType: "step", ParentID: &nodeB.ID, SortOrder: 200}
	db.Create(&nodeC2)

	db.Create(&data_models.GoalEdge{TreeID: tree.ID, FromID: nodeA.ID, ToID: nodeB.ID})
	db.Create(&data_models.GoalEdge{TreeID: tree.ID, FromID: nodeB.ID, ToID: nodeC1.ID})
	db.Create(&data_models.GoalEdge{TreeID: tree.ID, FromID: nodeB.ID, ToID: nodeC2.ID})

	router := setupNodeRouter(db, 1)

	// Delete B — C1 and C2 should attach to A
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/goals/trees/%d/nodes/%d", tree.ID, nodeB.ID), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// C1 parent should be A
	var c1 data_models.GoalNode
	db.Where("id = ?", nodeC1.ID).First(&c1)
	if c1.ParentID == nil || *c1.ParentID != nodeA.ID {
		t.Errorf("C1 parent should be A, got %v", c1.ParentID)
	}

	// C2 parent should be A
	var c2 data_models.GoalNode
	db.Where("id = ?", nodeC2.ID).First(&c2)
	if c2.ParentID == nil || *c2.ParentID != nodeA.ID {
		t.Errorf("C2 parent should be A, got %v", c2.ParentID)
	}

	// Edges A→C1 and A→C2 should exist
	var edges []data_models.GoalEdge
	db.Where("from_id = ?", nodeA.ID).Find(&edges)
	if len(edges) != 2 {
		t.Errorf("expected 2 edges from A, got %d", len(edges))
	}
}
