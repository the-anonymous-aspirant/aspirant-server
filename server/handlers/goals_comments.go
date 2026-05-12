package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

type createCommentRequest struct {
	Body string `json:"body" binding:"required"`
}

type updateCommentRequest struct {
	Body string `json:"body" binding:"required"`
}

type commentResponse struct {
	ID        uint      `json:"id"`
	NodeID    uint      `json:"node_id"`
	UserID    uint      `json:"user_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toCommentResponse(c *data_models.GoalComment) commentResponse {
	return commentResponse{
		ID:        c.ID,
		NodeID:    c.NodeID,
		UserID:    c.UserID,
		Body:      c.Body,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}

// getNodeForUser loads a node and verifies the authenticated user owns its tree.
func getNodeForUser(c *gin.Context, db *gorm.DB, userID uint, nodeIDParam string) *data_models.GoalNode {
	if nodeIDParam == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Node ID is required")
		return nil
	}

	var node data_models.GoalNode
	if err := db.Where("id = ? AND deleted_at IS NULL", nodeIDParam).First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return nil
	}

	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", node.TreeID, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return nil
	}

	return &node
}

// CreateCommentHandler creates a comment on a node.
func CreateCommentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	node := getNodeForUser(c, db, userID, c.Param("node_id"))
	if node == nil {
		return
	}

	var req createCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "bad_request", "Body is required")
		return
	}

	comment := data_models.GoalComment{
		NodeID: node.ID,
		UserID: userID,
		Body:   req.Body,
	}

	if err := db.Create(&comment).Error; err != nil {
		log.Printf("Error creating comment: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create comment")
		return
	}

	c.JSON(http.StatusCreated, toCommentResponse(&comment))
}

// ListCommentsHandler returns all non-deleted comments for a node.
func ListCommentsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	node := getNodeForUser(c, db, userID, c.Param("node_id"))
	if node == nil {
		return
	}

	var comments []data_models.GoalComment
	if err := db.Where("node_id = ? AND deleted_at IS NULL", node.ID).
		Order("created_at ASC").
		Find(&comments).Error; err != nil {
		log.Printf("Error listing comments: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list comments")
		return
	}

	resp := make([]commentResponse, len(comments))
	for i := range comments {
		resp[i] = toCommentResponse(&comments[i])
	}

	c.JSON(http.StatusOK, resp)
}

// UpdateCommentHandler edits a comment's body and sets updated_at.
func UpdateCommentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	commentID := c.Param("id")
	if commentID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Comment ID is required")
		return
	}

	var comment data_models.GoalComment
	if err := db.Where("id = ? AND deleted_at IS NULL", commentID).First(&comment).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	// Verify user owns the tree containing this comment's node
	var node data_models.GoalNode
	if err := db.Where("id = ? AND deleted_at IS NULL", comment.NodeID).First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Comment not found")
		return
	}
	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", node.TreeID, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	var req updateCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "bad_request", "Body is required")
		return
	}

	comment.Body = req.Body
	comment.UpdatedAt = time.Now()

	if err := db.Save(&comment).Error; err != nil {
		log.Printf("Error updating comment: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to update comment")
		return
	}

	c.JSON(http.StatusOK, toCommentResponse(&comment))
}

// DeleteCommentHandler soft-deletes a comment.
func DeleteCommentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	commentID := c.Param("id")
	if commentID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Comment ID is required")
		return
	}

	var comment data_models.GoalComment
	if err := db.Where("id = ? AND deleted_at IS NULL", commentID).First(&comment).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	// Verify user owns the tree containing this comment's node
	var node data_models.GoalNode
	if err := db.Where("id = ? AND deleted_at IS NULL", comment.NodeID).First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Comment not found")
		return
	}
	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", node.TreeID, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	now := time.Now()
	comment.DeletedAt = &now
	if err := db.Save(&comment).Error; err != nil {
		log.Printf("Error deleting comment: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete comment")
		return
	}

	c.Status(http.StatusNoContent)
}
