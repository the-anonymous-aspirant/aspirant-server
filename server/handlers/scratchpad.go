package handlers

import (
	"log"
	"net/http"
	"strings"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// maxScratchpadBytes bounds the stored text. 1 MiB is generous for a plain-text
// scratchpad; a paste larger than this is rejected with a clear 413 rather than
// silently truncated.
const maxScratchpadBytes = 1 << 20 // 1 MiB

// putScratchpadRequest is the PUT body. Text is NOT binding:"required" — an
// empty string is a valid state (the user clearing their scratchpad).
type putScratchpadRequest struct {
	Text string `json:"text"`
}

// scratchpadResponse is the GET/PUT reply. UpdatedAt is a pointer so a
// never-written scratchpad serializes as {"text":"","updated_at":null}.
type scratchpadResponse struct {
	Text      string     `json:"text"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// GetScratchpadHandler returns the authenticated user's scratchpad. A user who
// has never written one gets an empty buffer (200, not 404). The scratchpad is
// resolved from the session user id only — never from a URL or body parameter.
func GetScratchpadHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	sp, err := data_models.GetScratchpad(db, userID)
	if err != nil {
		log.Printf("Error fetching scratchpad: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to fetch scratchpad")
		return
	}

	if sp == nil {
		c.JSON(http.StatusOK, scratchpadResponse{Text: "", UpdatedAt: nil})
		return
	}

	updated := sp.UpdatedAt
	c.JSON(http.StatusOK, scratchpadResponse{Text: sp.Content, UpdatedAt: &updated})
}

// PutScratchpadHandler overwrites the authenticated user's scratchpad. Bounded
// at maxScratchpadBytes server-side; scoped to the session user id only.
func PutScratchpadHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	userID, ok := getAuthUserID(c)
	if !ok {
		return
	}

	// Hard cap the request body before decoding to avoid buffering an abusive
	// payload. Headroom above the text ceiling covers the small JSON envelope;
	// the exact text-length check below returns the precise error.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxScratchpadBytes+4096)

	var req putScratchpadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			respondError(c, http.StatusRequestEntityTooLarge, "payload_too_large",
				"Scratchpad text exceeds the 1 MiB limit")
			return
		}
		respondError(c, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if len(req.Text) > maxScratchpadBytes {
		respondError(c, http.StatusRequestEntityTooLarge, "payload_too_large",
			"Scratchpad text exceeds the 1 MiB limit")
		return
	}

	sp, err := data_models.UpsertScratchpad(db, userID, req.Text)
	if err != nil {
		log.Printf("Error saving scratchpad: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to save scratchpad")
		return
	}

	updated := sp.UpdatedAt
	c.JSON(http.StatusOK, scratchpadResponse{Text: sp.Content, UpdatedAt: &updated})
}
