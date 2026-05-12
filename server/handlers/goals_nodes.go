package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

const maxNodeDepth = 5

type createNodeRequest struct {
	ParentID *uint  `json:"parent_id"`
	Name     string `json:"name" binding:"required"`
	NodeType string `json:"node_type" binding:"required"`
	Color    string `json:"color"`
	Body     string `json:"body"`
}

type updateNodeRequest struct {
	Name     *string `json:"name"`
	NodeType *string `json:"node_type"`
	Color    *string `json:"color"`
	Body     *string `json:"body"`
}

type nodeResponse struct {
	ID             uint       `json:"id"`
	TreeID         uint       `json:"tree_id"`
	ParentID       *uint      `json:"parent_id"`
	Name           string     `json:"name"`
	NodeType       string     `json:"node_type"`
	Color          string     `json:"color"`
	Body           string     `json:"body"`
	SortOrder      int        `json:"sort_order"`
	PlannedStart   *time.Time `json:"planned_start,omitempty"`
	PlannedEnd     *time.Time `json:"planned_end,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	ManualComplete bool       `json:"manual_complete"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func toNodeResponse(n *data_models.GoalNode) nodeResponse {
	return nodeResponse{
		ID:             n.ID,
		TreeID:         n.TreeID,
		ParentID:       n.ParentID,
		Name:           n.Name,
		NodeType:       n.NodeType,
		Color:          n.Color,
		Body:           n.Body,
		SortOrder:      n.SortOrder,
		PlannedStart:   n.PlannedStart,
		PlannedEnd:     n.PlannedEnd,
		CompletedAt:    n.CompletedAt,
		ManualComplete: n.ManualComplete,
		CreatedAt:      n.CreatedAt,
		UpdatedAt:      n.UpdatedAt,
	}
}

// getTreeForUser loads a tree scoped to the authenticated user.
// Returns nil and writes an HTTP error if not found.
func getTreeForUser(c *gin.Context, db *gorm.DB, userID uint) *data_models.GoalTree {
	treeID := c.Param("tree_id")
	if treeID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Tree ID is required")
		return nil
	}

	var tree data_models.GoalTree
	if err := db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", treeID, userID).
		First(&tree).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Tree not found")
		return nil
	}
	return &tree
}

// computeDepth walks up the parent chain starting from parentID.
// Returns the depth (0-indexed: root is depth 0, its child is depth 1, etc).
func computeDepth(db *gorm.DB, treeID uint, parentID *uint) int {
	if parentID == nil {
		return 0
	}

	depth := 1
	currentID := *parentID
	for {
		var node data_models.GoalNode
		if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", currentID, treeID).
			First(&node).Error; err != nil {
			break
		}
		if node.ParentID == nil {
			break
		}
		depth++
		currentID = *node.ParentID
	}
	return depth
}

// maxSortOrder returns the current maximum sort_order among siblings.
func maxSortOrder(db *gorm.DB, treeID uint, parentID *uint) int {
	var max struct{ Max *int }
	query := db.Model(&data_models.GoalNode{}).
		Where("tree_id = ? AND deleted_at IS NULL", treeID)

	if parentID == nil {
		query = query.Where("parent_id IS NULL")
	} else {
		query = query.Where("parent_id = ?", *parentID)
	}

	query.Select("MAX(sort_order) as max").Scan(&max)
	if max.Max == nil {
		return 0
	}
	return *max.Max
}

// CreateNodeHandler creates a new node within a tree.
// Validates depth (max 5) and auto-assigns sort_order.
func CreateNodeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	var req createNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "bad_request", "Name and node_type are required")
		return
	}

	// Validate parent exists in this tree if specified
	if req.ParentID != nil {
		var parent data_models.GoalNode
		if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", *req.ParentID, tree.ID).
			First(&parent).Error; err != nil {
			respondError(c, http.StatusBadRequest, "bad_request", "Parent node not found in this tree")
			return
		}
	}

	// Depth check: the new node would be at depth = computeDepth + 1 for children of existing nodes
	depth := computeDepth(db, tree.ID, req.ParentID)
	if depth >= maxNodeDepth {
		respondError(c, http.StatusUnprocessableEntity, "max_depth_exceeded",
			"Maximum tree depth of 5 levels exceeded")
		return
	}

	sortOrder := maxSortOrder(db, tree.ID, req.ParentID) + 100

	node := data_models.GoalNode{
		TreeID:    tree.ID,
		ParentID:  req.ParentID,
		Name:      req.Name,
		NodeType:  req.NodeType,
		Color:     req.Color,
		Body:      req.Body,
		SortOrder: sortOrder,
	}

	tx := db.Begin()

	if err := tx.Create(&node).Error; err != nil {
		tx.Rollback()
		log.Printf("Error creating node: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create node")
		return
	}

	// Create edge from parent to this node
	if req.ParentID != nil {
		edge := data_models.GoalEdge{
			TreeID: tree.ID,
			FromID: *req.ParentID,
			ToID:   node.ID,
		}
		if err := tx.Create(&edge).Error; err != nil {
			tx.Rollback()
			log.Printf("Error creating edge: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to create node")
			return
		}
	}

	// If this is the first root node, set it as the tree's root_node_id
	if req.ParentID == nil && tree.RootNodeID == nil {
		tree.RootNodeID = &node.ID
		if err := tx.Save(tree).Error; err != nil {
			tx.Rollback()
			log.Printf("Error updating tree root_node_id: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to create node")
			return
		}
	}

	tx.Commit()
	c.JSON(http.StatusCreated, toNodeResponse(&node))
}

// ListNodesHandler returns all non-deleted nodes for a tree.
// Supports optional timeline filtering via query parameters:
//   ?period=day|week|month|quarter|year|custom
//   &value=<period-specific value>
//   &mode=planned|achieved|combined (default: planned)
func ListNodesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	periodRange, mode, err := parseTimelineParams(c)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_period", err.Error())
		return
	}

	query := db.Where("tree_id = ? AND deleted_at IS NULL", tree.ID)
	if periodRange != nil {
		query = applyTimelineFilter(query, periodRange, mode)
	}

	var nodes []data_models.GoalNode
	if err := query.Order("sort_order ASC").Find(&nodes).Error; err != nil {
		log.Printf("Error listing nodes: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list nodes")
		return
	}

	resp := make([]nodeResponse, len(nodes))
	for i := range nodes {
		resp[i] = toNodeResponse(&nodes[i])
	}

	c.JSON(http.StatusOK, resp)
}

// GetNodeHandler returns a single node by ID within a tree.
func GetNodeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	nodeID := c.Param("id")
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Node ID is required")
		return
	}

	var node data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", nodeID, tree.ID).
		First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return
	}

	c.JSON(http.StatusOK, toNodeResponse(&node))
}

// UpdateNodeHandler partially updates a node's fields.
func UpdateNodeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	nodeID := c.Param("id")
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Node ID is required")
		return
	}

	var node data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", nodeID, tree.ID).
		First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return
	}

	var req updateNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if req.Name != nil {
		node.Name = *req.Name
	}
	if req.NodeType != nil {
		node.NodeType = *req.NodeType
	}
	if req.Color != nil {
		node.Color = *req.Color
	}
	if req.Body != nil {
		node.Body = *req.Body
	}

	if err := db.Save(&node).Error; err != nil {
		log.Printf("Error updating node: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to update node")
		return
	}

	c.JSON(http.StatusOK, toNodeResponse(&node))
}

// DeleteNodeHandler soft-deletes a node and reattaches its children to its parent.
// In A→B→C, deleting B results in A→C (edge survival).
func DeleteNodeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	nodeID := c.Param("id")
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Node ID is required")
		return
	}

	var node data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", nodeID, tree.ID).
		First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return
	}

	tx := db.Begin()
	now := time.Now()

	// Find children of this node (nodes where parent_id = this node's ID)
	var children []data_models.GoalNode
	if err := tx.Where("parent_id = ? AND tree_id = ? AND deleted_at IS NULL", node.ID, tree.ID).
		Find(&children).Error; err != nil {
		tx.Rollback()
		log.Printf("Error finding children: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
		return
	}

	// Reattach children to this node's parent
	if len(children) > 0 {
		for _, child := range children {
			child.ParentID = node.ParentID
			if err := tx.Save(&child).Error; err != nil {
				tx.Rollback()
				log.Printf("Error reattaching child %d: %v", child.ID, err)
				RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
				return
			}
		}

		// Delete edges FROM this node to its children
		if err := tx.Where("from_id = ? AND tree_id = ?", node.ID, tree.ID).
			Delete(&data_models.GoalEdge{}).Error; err != nil {
			tx.Rollback()
			log.Printf("Error deleting child edges: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
			return
		}

		// Create new edges from this node's parent to each child
		if node.ParentID != nil {
			for _, child := range children {
				edge := data_models.GoalEdge{
					TreeID: tree.ID,
					FromID: *node.ParentID,
					ToID:   child.ID,
				}
				if err := tx.Create(&edge).Error; err != nil {
					tx.Rollback()
					log.Printf("Error creating replacement edge: %v", err)
					RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
					return
				}
			}
		}
	}

	// Delete edges TO this node (from parent)
	if err := tx.Where("to_id = ? AND tree_id = ?", node.ID, tree.ID).
		Delete(&data_models.GoalEdge{}).Error; err != nil {
		tx.Rollback()
		log.Printf("Error deleting parent edge: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
		return
	}

	// Soft-delete comments on this node
	if err := tx.Model(&data_models.GoalComment{}).
		Where("node_id = ? AND deleted_at IS NULL", node.ID).
		Update("deleted_at", now).Error; err != nil {
		tx.Rollback()
		log.Printf("Error soft-deleting comments: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
		return
	}

	// If this node was the tree root, update root_node_id
	if tree.RootNodeID != nil && *tree.RootNodeID == node.ID {
		// If root had exactly one child, promote it to root
		if len(children) == 1 {
			tree.RootNodeID = &children[0].ID
		} else {
			tree.RootNodeID = nil
		}
		if err := tx.Save(tree).Error; err != nil {
			tx.Rollback()
			log.Printf("Error updating tree root: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
			return
		}
	}

	// Soft-delete the node
	node.DeletedAt = &now
	if err := tx.Save(&node).Error; err != nil {
		tx.Rollback()
		log.Printf("Error soft-deleting node: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete node")
		return
	}

	tx.Commit()
	c.Status(http.StatusNoContent)
}

type completeNodeRequest struct {
	ManualComplete *bool `json:"manual_complete"`
}

// allChildrenCompleted checks whether every direct child of parentID is completed.
func allChildrenCompleted(db *gorm.DB, treeID uint, parentID uint) bool {
	var incomplete int
	db.Model(&data_models.GoalNode{}).
		Where("parent_id = ? AND tree_id = ? AND deleted_at IS NULL AND completed_at IS NULL", parentID, treeID).
		Count(&incomplete)
	return incomplete == 0
}

// hasChildren checks whether the node has any non-deleted children.
func hasChildren(db *gorm.DB, treeID uint, nodeID uint) bool {
	var count int
	db.Model(&data_models.GoalNode{}).
		Where("parent_id = ? AND tree_id = ? AND deleted_at IS NULL", nodeID, treeID).
		Count(&count)
	return count > 0
}

// rollupComplete cascades auto-completion upward from nodeID.
// When all siblings are completed, the parent gets auto-completed (unless manual_complete is set).
func rollupComplete(db *gorm.DB, treeID uint, parentID *uint, now time.Time) {
	if parentID == nil {
		return
	}

	var parent data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", *parentID, treeID).
		First(&parent).Error; err != nil {
		return
	}

	if parent.CompletedAt != nil {
		return
	}

	if parent.ManualComplete {
		return
	}

	if !allChildrenCompleted(db, treeID, parent.ID) {
		return
	}

	parent.CompletedAt = &now
	db.Save(&parent)

	rollupComplete(db, treeID, parent.ParentID, now)
}

// rollupUncomplete cascades uncomplete upward.
// If a parent was auto-completed (manual_complete=false), clear its completed_at.
func rollupUncomplete(db *gorm.DB, treeID uint, parentID *uint) {
	if parentID == nil {
		return
	}

	var parent data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", *parentID, treeID).
		First(&parent).Error; err != nil {
		return
	}

	if parent.CompletedAt == nil {
		return
	}

	if parent.ManualComplete {
		return
	}

	parent.CompletedAt = nil
	db.Save(&parent)

	rollupUncomplete(db, treeID, parent.ParentID)
}

// CompleteNodeHandler marks a node as completed and cascades auto-completion upward.
func CompleteNodeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	nodeID := c.Param("id")
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Node ID is required")
		return
	}

	var node data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", nodeID, tree.ID).
		First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return
	}

	if node.CompletedAt != nil {
		respondError(c, http.StatusConflict, "already_completed", "Node is already completed")
		return
	}

	var req completeNodeRequest
	c.ShouldBindJSON(&req)

	now := time.Now()
	node.CompletedAt = &now
	if req.ManualComplete != nil && *req.ManualComplete {
		node.ManualComplete = true
	}

	if err := db.Save(&node).Error; err != nil {
		log.Printf("Error completing node: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to complete node")
		return
	}

	rollupComplete(db, tree.ID, node.ParentID, now)

	c.JSON(http.StatusOK, toNodeResponse(&node))
}

// UncompleteNodeHandler clears the completed_at timestamp and propagates upward.
func UncompleteNodeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	tree := getTreeForUser(c, db, userID)
	if tree == nil {
		return
	}

	nodeID := c.Param("id")
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "bad_request", "Node ID is required")
		return
	}

	var node data_models.GoalNode
	if err := db.Where("id = ? AND tree_id = ? AND deleted_at IS NULL", nodeID, tree.ID).
		First(&node).Error; err != nil {
		respondError(c, http.StatusNotFound, "not_found", "Node not found")
		return
	}

	if node.CompletedAt == nil {
		respondError(c, http.StatusConflict, "not_completed", "Node is not completed")
		return
	}

	node.CompletedAt = nil
	node.ManualComplete = false
	if err := db.Save(&node).Error; err != nil {
		log.Printf("Error uncompleting node: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to uncomplete node")
		return
	}

	rollupUncomplete(db, tree.ID, node.ParentID)

	c.JSON(http.StatusOK, toNodeResponse(&node))
}
