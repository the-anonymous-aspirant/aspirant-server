package handlers

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Profile endpoints (#4170) — the logged-in user's own profile surface.
//
// These live under /profile (not /data_models/users/me) on purpose: Gin's
// radix router cannot hold a static `me` segment beside the existing
// `/data_models/users/:id` wildcard without a route conflict. The self routes
// are all scoped to the session `user_id` from the JWT (AuthMiddleware) and
// never trust an id from the body or URL — a user can only read and mutate
// their own profile. The per-user avatar-serve route is the one exception that
// takes an :id, because any authenticated caller may render any user's avatar
// (e.g. the message-board author strip).

const (
	// maxAvatarBytes caps an uploaded profile picture. 2 MiB is generous for a
	// small square avatar and keeps a single upload well inside request limits.
	maxAvatarBytes = 2 << 20 // 2 MiB
	// maxDisplayNameLen bounds the editable display name (rune count).
	maxDisplayNameLen = 50
)

// allowedAvatarTypes is the set of image content types accepted for an avatar,
// checked against the server-sniffed type of the bytes (never the client's
// Content-Type header).
var allowedAvatarTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/gif":  true,
}

// MeResponse is the DTO returned by GET /profile — the caller's own identity.
// It exposes the caller's own email (self-view, not a cross-user leak), the
// current display name (temporal, from user_display_names), the member-since
// date (CreatedAt), and the browser-facing avatar URL ("" when none is set).
type MeResponse struct {
	ID          uint      `json:"ID"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email"`
	AvatarURL   string    `json:"avatar_url"`
	CreatedAt   time.Time `json:"CreatedAt"`
}

// callerUserID extracts the authenticated caller's user id set by
// AuthMiddleware. ok is false when the key is absent (route not behind auth).
func callerUserID(c *gin.Context) (uint, bool) {
	v, exists := c.Get("user_id")
	if !exists {
		return 0, false
	}
	id, ok := v.(uint)
	return id, ok
}

// GetMeHandler handles GET /profile — the caller's own profile.
func GetMeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	var user data_models.User
	if err := db.Where("id = ?", userID).First(&user).Error; err != nil {
		log.Printf("Profile: user %d not found: %v", userID, err)
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	resp := MeResponse{
		ID:          user.ID,
		Username:    user.Username,
		DisplayName: data_models.CurrentDisplayName(db, user.ID),
		Email:       user.Email,
		AvatarURL:   data_models.AvatarURLFor(user.ID, user.AvatarETag),
		CreatedAt:   user.CreatedAt,
	}
	RespondWithSuccess(c, resp, "Profile retrieved successfully")
}

// patchMeInput binds the mutable profile fields. Only display_name is editable
// here; the login Username is a credential and is never mutated from the
// profile surface (the display name is the public identity, per #3094).
type patchMeInput struct {
	DisplayName *string `json:"display_name"`
}

// PatchMeHandler handles PATCH /profile — update the caller's display name.
func PatchMeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	var input patchMeInput
	if err := c.ShouldBindJSON(&input); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid profile data")
		return
	}

	if input.DisplayName != nil {
		name := strings.TrimSpace(*input.DisplayName)
		if name == "" {
			RespondWithError(c, http.StatusBadRequest, "Display name cannot be empty")
			return
		}
		if len([]rune(name)) > maxDisplayNameLen {
			RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("Display name must be at most %d characters", maxDisplayNameLen))
			return
		}
		if err := data_models.SetDisplayName(db, userID, name); err != nil {
			log.Printf("Profile: set display name for user %d failed: %v", userID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to update display name")
			return
		}
	}

	// Re-read and return the fresh profile so the client re-renders from
	// authoritative state rather than echoing its own request.
	var user data_models.User
	if err := db.Where("id = ?", userID).First(&user).Error; err != nil {
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}
	resp := MeResponse{
		ID:          user.ID,
		Username:    user.Username,
		DisplayName: data_models.CurrentDisplayName(db, user.ID),
		Email:       user.Email,
		AvatarURL:   data_models.AvatarURLFor(user.ID, user.AvatarETag),
		CreatedAt:   user.CreatedAt,
	}
	RespondWithSuccess(c, resp, "Profile updated successfully")
}

// PutMeAvatarHandler handles PUT /profile/avatar — upload/replace the caller's
// avatar (multipart form field `image`). The image is stored content-addressed
// in asset storage and the resulting ETag recorded on the user row. The
// content type is sniffed from the bytes, not trusted from the client.
func PutMeAvatarHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	file, err := c.FormFile("image")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "No image file received")
		return
	}
	if file.Size > maxAvatarBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("Image exceeds the %d byte limit", maxAvatarBytes))
		return
	}

	f, err := file.Open()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read image")
		return
	}
	defer f.Close()

	content, err := io.ReadAll(io.LimitReader(f, maxAvatarBytes+1))
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read image")
		return
	}
	if len(content) > maxAvatarBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("Image exceeds the %d byte limit", maxAvatarBytes))
		return
	}
	if len(content) == 0 {
		RespondWithError(c, http.StatusBadRequest, "Image is empty")
		return
	}

	contentType := http.DetectContentType(content)
	if !allowedAvatarTypes[contentType] {
		RespondWithError(c, http.StatusBadRequest, "Unsupported image type (allowed: JPEG, PNG, WebP, GIF)")
		return
	}

	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return
	}
	assets := store.(*storage.LocalStorage)

	etag := fmt.Sprintf("%x", md5.Sum(content))
	key := "avatars/" + etag
	if err := assets.Put(key, bytes.NewReader(content)); err != nil {
		log.Printf("Profile: store avatar for user %d failed: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to store image")
		return
	}

	if err := db.Model(&data_models.User{}).Where("id = ?", userID).Update("avatar_etag", etag).Error; err != nil {
		log.Printf("Profile: record avatar etag for user %d failed: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to save avatar")
		return
	}

	RespondWithSuccess(c, gin.H{"avatar_url": data_models.AvatarURLFor(userID, etag)}, "Avatar updated successfully")
}

// DeleteMeAvatarHandler handles DELETE /profile/avatar — clear the caller's
// avatar. The stored blob is left in place (the store is content-addressed and
// a blob may be shared); only the pointer on the user row is cleared, so the
// placeholder fallback returns everywhere the avatar was rendered.
func DeleteMeAvatarHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	if err := db.Model(&data_models.User{}).Where("id = ?", userID).Update("avatar_etag", "").Error; err != nil {
		log.Printf("Profile: clear avatar for user %d failed: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to clear avatar")
		return
	}
	RespondWithSuccess(c, gin.H{"avatar_url": ""}, "Avatar cleared successfully")
}

// GetUserAvatarHandler handles GET /data_models/users/:id/avatar — serve a
// user's avatar bytes to any authenticated caller. This is the authenticated
// counterpart to the public /fetch-object gate (which only Trusted/Admin
// bypass): a plain User must be able to render their own and others' avatars on
// the profile page and message board. The URL is ETag-versioned (?v=), so the
// response is cacheable.
func GetUserAvatarHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "User ID is required")
		return
	}

	var user data_models.User
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		RespondWithError(c, http.StatusNotFound, "Avatar not found")
		return
	}
	if user.AvatarETag == "" {
		RespondWithError(c, http.StatusNotFound, "Avatar not found")
		return
	}

	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return
	}
	assets := store.(*storage.LocalStorage)

	data, info, err := assets.GetByETag(user.AvatarETag)
	if err != nil || data == nil {
		log.Printf("Profile: avatar bytes for user %s (etag %s) missing: %v", id, user.AvatarETag, err)
		RespondWithError(c, http.StatusNotFound, "Avatar not found")
		return
	}

	// The URL carries the content ETag as ?v=, so a given URL always maps to
	// the same bytes — safe to cache privately for a day.
	c.Header("Cache-Control", "private, max-age=86400")
	c.Data(http.StatusOK, info.ContentType, data)
}
