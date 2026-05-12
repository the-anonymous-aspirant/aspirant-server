package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

type createTreeRequest struct {
	Name string `json:"name" binding:"required"`
}

type updateTreeRequest struct {
	Name string `json:"name" binding:"required"`
}

type treeResponse struct {
	ID                         uint       `json:"id"`
	Name                       string     `json:"name"`
	RootNodeID                 *uint      `json:"root_node_id"`
	EditingSessionID           *string    `json:"editing_session_id,omitempty"`
	EditingSessionLastActivity *time.Time `json:"editing_session_last_activity,omitempty"`
	CreatedAt                  time.Time  `json:"created_at"`
	UpdatedAt                  time.Time  `json:"updated_at"`
	DeletedAt                  *time.Time `json:"deleted_at,omitempty"`
}

func toTreeResponse(t *data_models.GoalTree) treeResponse {
	return treeResponse{
		ID:                         t.ID,
		Name:                       t.Name,
		RootNodeID:                 t.RootNodeID,
		EditingSessionID:           t.EditingSessionID,
		EditingSessionLastActivity: t.EditingSessionLastActivity,
		CreatedAt:                  t.CreatedAt,
		UpdatedAt:                  t.UpdatedAt,
	}
}

func getAuthUserID(c *gin.Context) (uint, bool) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return 0, false
	}
	uid, ok := userID.(uint)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Invalid user identity")
		return 0, false
	}
	return uid, true
}

// CreateTreeHandler creates a new goal tree for the authenticated user.
func CreateTreeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	var req createTreeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}

	tree := data_models.GoalTree{
		UserID: userID,
		Name:   req.Name,
	}

	if err := db.Create(&tree).Error; err != nil {
		log.Printf("Error creating tree: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create tree")
		return
	}

	c.JSON(http.StatusCreated, toTreeResponse(&tree))
}

// ListTreesHandler returns all non-deleted trees for the authenticated user.
func ListTreesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	var trees []data_models.GoalTree
	if err := db.Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("created_at DESC").
		Find(&trees).Error; err != nil {
		log.Printf("Error listing trees: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list trees")
		return
	}

	resp := make([]treeResponse, len(trees))
	for i := range trees {
		resp[i] = toTreeResponse(&trees[i])
	}

	c.JSON(http.StatusOK, resp)
}

// GetTreeHandler returns a single tree by ID, scoped to the authenticated user.
// Returns 404 if the tree belongs to another user (no existence leakage).
func GetTreeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	id := c.Param("id")
	if id == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Tree ID is required")
		return
	}

	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Tree not found")
		return
	}

	c.JSON(http.StatusOK, toTreeResponse(&tree))
}

// UpdateTreeHandler updates the name of an existing tree.
func UpdateTreeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	id := c.Param("id")
	if id == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Tree ID is required")
		return
	}

	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Tree not found")
		return
	}

	if !ValidateEditingSession(c, db, &tree) {
		return
	}

	var req updateTreeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}

	tree.Name = req.Name
	if err := db.Save(&tree).Error; err != nil {
		log.Printf("Error updating tree: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to update tree")
		return
	}

	c.JSON(http.StatusOK, toTreeResponse(&tree))
}

// DeleteTreeHandler soft-deletes a tree by setting deleted_at.
// Also soft-deletes all nodes, edges, and comments belonging to the tree.
func DeleteTreeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	id := c.Param("id")
	if id == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Tree ID is required")
		return
	}

	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Tree not found")
		return
	}

	now := time.Now()

	tx := db.Begin()

	if err := tx.Model(&data_models.GoalNode{}).
		Where("tree_id = ? AND deleted_at IS NULL", tree.ID).
		Update("deleted_at", now).Error; err != nil {
		tx.Rollback()
		log.Printf("Error soft-deleting nodes: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete tree")
		return
	}

	if err := tx.Where("tree_id = ?", tree.ID).Delete(&data_models.GoalEdge{}).Error; err != nil {
		tx.Rollback()
		log.Printf("Error deleting edges: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete tree")
		return
	}

	// Soft-delete comments on nodes belonging to this tree
	if err := tx.Exec(`UPDATE goal_comments SET deleted_at = ?
		WHERE node_id IN (SELECT id FROM goal_nodes WHERE tree_id = ?) AND deleted_at IS NULL`,
		now, tree.ID).Error; err != nil {
		tx.Rollback()
		log.Printf("Error soft-deleting comments: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete tree")
		return
	}

	tree.DeletedAt = &now
	if err := tx.Save(&tree).Error; err != nil {
		tx.Rollback()
		log.Printf("Error soft-deleting tree: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete tree")
		return
	}

	tx.Commit()
	c.Status(http.StatusNoContent)
}
