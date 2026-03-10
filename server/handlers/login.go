package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_models"
	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// LoginHandler handles user login
func LoginHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input struct {
		UserName string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		log.Printf("Invalid login input: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid login credentials")
		return
	}

	var user data_models.User
	if err := db.Preload("Role").Where("username= ?", input.UserName).First(&user).Error; err != nil {
		// Use generic message for security - don't specify if username or password was incorrect
		log.Printf("Failed login attempt for username: %s", input.UserName)
		RespondWithError(c, http.StatusUnauthorized, "Invalid login credentials")
		return
	}

	if err := user.CheckPassword(input.Password); err != nil {
		log.Printf("Invalid password for username: %s", input.UserName)
		RespondWithError(c, http.StatusUnauthorized, "Invalid login credentials")
		return
	}

	// Generate JWT token
	token, err := middleware.GenerateToken(user.ID, user.Role.RoleName)
	if err != nil {
		log.Printf("Failed to generate token: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Authentication error")
		return
	}

	log.Printf("Successful login for user: %s with role: %s, token: %s", user.Username, user.Role.RoleName, token)

	c.Set("user_name", user.Username)
	c.Set("user_id", user.ID)
	c.Set("role", user.Role.RoleName)
	c.Set("token", token)

	RespondWithSuccess(c, gin.H{
		"token":    token,
		"username": user.Username,
		"role":     user.Role.RoleName,
	}, "Login successful")
}
