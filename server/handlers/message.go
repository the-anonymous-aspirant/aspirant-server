package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// GetAllMessagesHandler handles retrieving all messages
func GetAllMessagesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	messages, err := data_models.GetAllMessages(db)
	if err != nil {
		log.Printf("Error retrieving messages: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving messages")
		return
	}

	RespondWithSuccess(c, messages, "Messages retrieved successfully")
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
