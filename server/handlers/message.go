package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// GetAllMessagesHandler handles retrieving all messages with pagination
func GetAllMessagesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	page, pageSize := parsePagination(c)
	offset := (page - 1) * pageSize

	var total int64
	if err := db.Model(&data_models.Message{}).Count(&total).Error; err != nil {
		log.Printf("Error counting messages: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving messages")
		return
	}

	var messages []data_models.Message
	if err := db.Order("updated_at desc").Offset(offset).Limit(pageSize).Find(&messages).Error; err != nil {
		log.Printf("Error retrieving messages: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving messages")
		return
	}

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    messages,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// PostMessageHandler handles posting a new message
func PostMessageHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var msg data_models.Message
	if err := c.ShouldBindJSON(&msg); err != nil {
		log.Printf("Invalid message data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid message data")
		return
	}

	// Validate message content
	if msg.Content == "" {
		RespondWithError(c, http.StatusBadRequest, "Message content is required")
		return
	}

	// Get user ID from context if available
	userID, exists := c.Get("user_id")
	if exists {
		msg.SenderID = userID.(uint)
	} else {
		// Anonymous user
		msg.SenderID = 0 // 0 represents anonymous user
	}

	msg.SentAt = time.Now()

	err := msg.Create(db)
	if err != nil {
		log.Printf("Error creating message: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating message")
		return
	}

	RespondWithSuccess(c, msg, "Message created successfully")
}
