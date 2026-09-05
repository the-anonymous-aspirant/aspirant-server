package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

const sessionLockTimeout = 30 * time.Minute

type openTreeResponse struct {
	TreeID           uint   `json:"tree_id"`
	EditingSessionID string `json:"editing_session_id"`
	Status           string `json:"status"`
}

type lockedElsewhereResponse struct {
	TreeID            uint   `json:"tree_id"`
	Status            string `json:"status"`
	ExistingSessionID string `json:"existing_session_id"`
	CanTakeOver       bool   `json:"can_take_over"`
	LastActivityAt    string `json:"last_activity_at,omitempty"`
}

func generateSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func isSessionExpired(lastActivity *time.Time) bool {
	if lastActivity == nil {
		return true
	}
	return time.Since(*lastActivity) > sessionLockTimeout
}

// OpenTreeForEditingHandler acquires an editing session lock on a tree.
// If the tree is already locked by another session (not expired), returns 409
// with details about the existing lock.
func OpenTreeForEditingHandler(c *gin.Context) {
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

	if tree.EditingSessionID != nil && !isSessionExpired(tree.EditingSessionLastActivity) {
		c.JSON(http.StatusConflict, lockedElsewhereResponse{
			TreeID:            tree.ID,
			Status:            "locked_elsewhere",
			ExistingSessionID: *tree.EditingSessionID,
			CanTakeOver:       true,
			LastActivityAt:    tree.EditingSessionLastActivity.UTC().Format(time.RFC3339),
		})
		return
	}

	// Acquire (or re-acquire expired) lock
	sessionID := generateSessionID()
	now := time.Now()
	tree.EditingSessionID = &sessionID
	tree.EditingSessionLastActivity = &now

	if err := db.Save(&tree).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to acquire editing lock")
		return
	}

	c.JSON(http.StatusOK, openTreeResponse{
		TreeID:           tree.ID,
		EditingSessionID: sessionID,
		Status:           "acquired",
	})
}

// TakeOverTreeEditingHandler forcibly takes over the editing session from another tab/device.
// Invalidates the prior session — subsequent writes from the old session return 409.
func TakeOverTreeEditingHandler(c *gin.Context) {
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

	sessionID := generateSessionID()
	now := time.Now()
	tree.EditingSessionID = &sessionID
	tree.EditingSessionLastActivity = &now

	if err := db.Save(&tree).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to take over editing session")
		return
	}

	c.JSON(http.StatusOK, openTreeResponse{
		TreeID:           tree.ID,
		EditingSessionID: sessionID,
		Status:           "taken_over",
	})
}

// ReleaseTreeEditingHandler explicitly releases the editing lock.
func ReleaseTreeEditingHandler(c *gin.Context) {
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

	// Only release if caller owns the session
	sessionID := c.GetHeader("X-Editing-Session-ID")
	if tree.EditingSessionID != nil && sessionID != *tree.EditingSessionID {
		respondError(c, http.StatusConflict, "session_mismatch", "Cannot release a session you don't own")
		return
	}

	tree.EditingSessionID = nil
	tree.EditingSessionLastActivity = nil

	if err := db.Save(&tree).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to release editing lock")
		return
	}

	c.Status(http.StatusNoContent)
}

// ValidateEditingSession checks that the request carries the correct editing session ID
// for write operations on a tree. Returns 409 if the session has been superseded.
// Also touches the last_activity timestamp on valid requests.
func ValidateEditingSession(c *gin.Context, db *gorm.DB, tree *data_models.GoalTree) bool {
	if tree.EditingSessionID == nil {
		return true
	}

	if isSessionExpired(tree.EditingSessionLastActivity) {
		tree.EditingSessionID = nil
		tree.EditingSessionLastActivity = nil
		db.Save(tree)
		return true
	}

	sessionID := c.GetHeader("X-Editing-Session-ID")
	if sessionID == "" || sessionID != *tree.EditingSessionID {
		respondError(c, http.StatusConflict, "session_superseded",
			"Editing session has been taken over by another client")
		return false
	}

	// Touch last activity
	now := time.Now()
	tree.EditingSessionLastActivity = &now
	db.Model(tree).Update("editing_session_last_activity", now)

	return true
}
