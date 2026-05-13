package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// TestAuthBoundary_UserA_CannotMutate_UserB_Trees verifies that cross-user
// requests return 404 (not 403) for all mutation methods, preventing existence
// leakage per spec §3.
func TestAuthBoundary_UserA_CannotMutate_UserB_Trees(t *testing.T) {
	db := setupTestDB(t)

	const userA uint = 1
	const userB uint = 2

	// User B creates a tree with a node
	treeB := data_models.GoalTree{UserID: userB, Name: "B's Private Tree"}
	db.Create(&treeB)

	nodeB := data_models.GoalNode{
		TreeID:   treeB.ID,
		Name:     "B's Goal",
		NodeType: "goal",
	}
	db.Create(&nodeB)

	// User A's router — all requests come from user A
	routerA := setupTreeRouterWithUser(db, userA)
	nodeRouterA := setupNodeRouter(db, userA)

	t.Run("POST_create_node_on_other_users_tree_returns_404", func(t *testing.T) {
		body := `{"name": "Injected Node", "type": "goal"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST",
			fmt.Sprintf("/goals/trees/%d/nodes", treeB.ID),
			bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		nodeRouterA.ServeHTTP(w, req)

		assertAuthBoundary404(t, w, "POST /nodes")
	})

	t.Run("PATCH_update_other_users_tree_returns_404", func(t *testing.T) {
		body := `{"name": "Hijacked"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PATCH",
			fmt.Sprintf("/goals/trees/%d", treeB.ID),
			bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		routerA.ServeHTTP(w, req)

		assertAuthBoundary404(t, w, "PATCH /trees/:id")
	})

	t.Run("DELETE_other_users_tree_returns_404", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE",
			fmt.Sprintf("/goals/trees/%d", treeB.ID), nil)
		routerA.ServeHTTP(w, req)

		assertAuthBoundary404(t, w, "DELETE /trees/:id")
	})

	t.Run("POST_complete_node_on_other_users_tree_returns_404", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST",
			fmt.Sprintf("/goals/trees/%d/nodes/%d/complete", treeB.ID, nodeB.ID), nil)
		nodeRouterA.ServeHTTP(w, req)

		assertAuthBoundary404(t, w, "POST /nodes/:id/complete")
	})

	// Verify user B's data remains untouched
	t.Run("userB_data_unchanged", func(t *testing.T) {
		var tree data_models.GoalTree
		db.Where("id = ?", treeB.ID).First(&tree)
		if tree.Name != "B's Private Tree" {
			t.Errorf("tree name was mutated: got %q", tree.Name)
		}
		if tree.DeletedAt != nil {
			t.Error("tree was deleted")
		}

		var node data_models.GoalNode
		db.Where("id = ?", nodeB.ID).First(&node)
		if node.CompletedAt != nil {
			t.Error("node was completed by unauthorized user")
		}
	})
}

// TestAuthBoundary_Bidirectional verifies the boundary works in both directions:
// user A cannot see B's trees AND user B cannot see A's trees.
func TestAuthBoundary_Bidirectional(t *testing.T) {
	db := setupTestDB(t)

	const userA uint = 5
	const userB uint = 10

	treeA := data_models.GoalTree{UserID: userA, Name: "A's Tree"}
	db.Create(&treeA)
	treeB := data_models.GoalTree{UserID: userB, Name: "B's Tree"}
	db.Create(&treeB)

	// User A cannot GET user B's tree
	routerA := setupTreeRouterWithUser(db, userA)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/goals/trees/%d", treeB.ID), nil)
	routerA.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("user A accessing B's tree: expected 404, got %d", w.Code)
	}

	// User B cannot GET user A's tree
	routerB := setupTreeRouterWithUser(db, userB)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", fmt.Sprintf("/goals/trees/%d", treeA.ID), nil)
	routerB.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("user B accessing A's tree: expected 404, got %d", w.Code)
	}

	// Each user sees only their own in LIST
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/goals/trees", nil)
	routerA.ServeHTTP(w, req)
	var treesA []treeResponse
	json.Unmarshal(w.Body.Bytes(), &treesA)
	if len(treesA) != 1 || treesA[0].Name != "A's Tree" {
		t.Errorf("user A list: expected [A's Tree], got %v", treesA)
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/goals/trees", nil)
	routerB.ServeHTTP(w, req)
	var treesB []treeResponse
	json.Unmarshal(w.Body.Bytes(), &treesB)
	if len(treesB) != 1 || treesB[0].Name != "B's Tree" {
		t.Errorf("user B list: expected [B's Tree], got %v", treesB)
	}
}

func assertAuthBoundary404(t *testing.T, w *httptest.ResponseRecorder, method string) {
	t.Helper()
	if w.Code != http.StatusNotFound {
		t.Fatalf("%s: expected 404, got %d: %s", method, w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("%s: failed to parse error response: %v", method, err)
	}
	if errResp.Error.Code != "not_found" {
		t.Errorf("%s: expected error code 'not_found', got '%s'", method, errResp.Error.Code)
	}
}
