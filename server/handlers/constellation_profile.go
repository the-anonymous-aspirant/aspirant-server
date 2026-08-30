package handlers

import (
	"log"
	"net/http"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// constellationProfileResponse is the caller's game identity: the game username
// (distinct from the login) plus the reused avatar URL (the user icon reuses
// the existing avatar rather than a separate uploader — epic #4587, #4595-A3).
type constellationProfileResponse struct {
	GameUsername string `json:"game_username"`
	AvatarURL    string `json:"avatar_url"`
}

type setConstellationProfileRequest struct {
	GameUsername string `json:"game_username"`
}

// GetConstellationProfileHandler returns the caller's Constellations game
// identity. Scoped to the session user id, never a path/body parameter. An
// unset profile returns an empty game_username, not an error.
func GetConstellationProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	var user data_models.User
	if err := db.Where("id = ?", userID).First(&user).Error; err != nil {
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	profile, _ := data_models.GetConstellationProfile(db, userID)
	c.JSON(http.StatusOK, constellationProfileResponse{
		GameUsername: profile.GameUsername,
		AvatarURL:    data_models.AvatarURLFor(user.ID, user.AvatarETag),
	})
}

// PutConstellationProfileHandler sets the caller's game username (the editable
// per-user default). Scoped to the session user id.
func PutConstellationProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	var req setConstellationProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	name := strings.TrimSpace(req.GameUsername)
	if name == "" {
		RespondWithError(c, http.StatusBadRequest, "game_username is required")
		return
	}
	if len([]rune(name)) > 40 {
		RespondWithError(c, http.StatusBadRequest, "game_username too long (max 40)")
		return
	}

	if _, err := data_models.SetConstellationUsername(db, userID, name); err != nil {
		log.Printf("Error setting constellation username for user %d: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving game username")
		return
	}

	var user data_models.User
	db.Where("id = ?", userID).First(&user)
	c.JSON(http.StatusOK, constellationProfileResponse{
		GameUsername: name,
		AvatarURL:    data_models.AvatarURLFor(user.ID, user.AvatarETag),
	})
}
