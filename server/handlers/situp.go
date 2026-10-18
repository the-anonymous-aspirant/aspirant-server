package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Pappas sit-ups challenge window (system_3 #7992): 57 daily rows from start
// (inclusive) to end (inclusive). The frontend's three reference lines assume
// 60 sit-ups a day across exactly this span.
var (
	situpChallengeStart = time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	situpChallengeEnd   = time.Date(2026, 12, 13, 0, 0, 0, 0, time.UTC)
)

// situpEditWindowDays mirrors the pushups rule: today, the 2 preceding and
// the 2 following days are editable.
const situpEditWindowDays = 2

// situpNow returns the current UTC time. Tests swap this to drive the
// edit-window rule deterministically.
var situpNow = func() time.Time { return time.Now().UTC() }

func inSitupChallengeWindow(date time.Time) bool {
	d := truncateToDay(date)
	return !d.Before(situpChallengeStart) && !d.After(situpChallengeEnd)
}

func inSitupEditWindow(date time.Time, now time.Time) bool {
	d := truncateToDay(date)
	today := truncateToDay(now)
	diff := int(d.Sub(today).Hours() / 24)
	if diff < 0 {
		diff = -diff
	}
	return diff <= situpEditWindowDays
}

type situpEntryDTO struct {
	Date      string `json:"date"`
	Count     *int   `json:"count"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy uint   `json:"updated_by,omitempty"`
}

// gapFillSitupEntries returns one DTO per day in the challenge window,
// populated from saved rows when present and otherwise as a null-count
// placeholder, so the frontend always receives the full 57-row table.
func gapFillSitupEntries(saved []data_models.SitupEntry) []situpEntryDTO {
	byDate := make(map[string]data_models.SitupEntry, len(saved))
	for _, e := range saved {
		byDate[truncateToDay(e.Date).Format(pushupDateLayout)] = e
	}
	out := make([]situpEntryDTO, 0, 57)
	for d := situpChallengeStart; !d.After(situpChallengeEnd); d = d.AddDate(0, 0, 1) {
		key := d.Format(pushupDateLayout)
		if e, ok := byDate[key]; ok {
			out = append(out, situpEntryDTO{
				Date:      key,
				Count:     e.Count,
				UpdatedAt: e.UpdatedAt.UTC().Format(time.RFC3339),
				UpdatedBy: e.UpdatedBy,
			})
			continue
		}
		out = append(out, situpEntryDTO{Date: key, Count: nil})
	}
	return out
}

// GetSitupEntriesHandler returns every day of the window, gap-filled.
func GetSitupEntriesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	saved, err := data_models.GetSitupEntries(db)
	if err != nil {
		log.Printf("Error retrieving situp entries: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving situp entries")
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": gapFillSitupEntries(saved)})
}

type patchSitupEntryRequest struct {
	Count *int `json:"count"`
}

// PatchSitupEntryHandler upserts a single date. As with pushups, the server
// is the gate for both windows; the frontend only mirrors them for UX.
func PatchSitupEntryHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	date, err := time.Parse(pushupDateLayout, c.Param("date"))
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid date format, expected YYYY-MM-DD")
		return
	}
	date = truncateToDay(date)

	if !inSitupChallengeWindow(date) {
		RespondWithError(c, http.StatusBadRequest, "Date outside challenge window (2026-10-18..2026-12-13)")
		return
	}
	if !inSitupEditWindow(date, situpNow()) {
		RespondWithError(c, http.StatusForbidden, "Date outside edit window (current_date ± 2 days)")
		return
	}

	var req patchSitupEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Count != nil && *req.Count < 0 {
		RespondWithError(c, http.StatusBadRequest, "Count must be non-negative")
		return
	}

	var userID uint
	if v, exists := c.Get("user_id"); exists {
		userID, _ = v.(uint)
	}

	entry, err := data_models.UpsertSitupEntry(db, date, req.Count, userID)
	if err != nil {
		log.Printf("Error upserting situp entry: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving situp entry")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"entry": situpEntryDTO{
			Date:      truncateToDay(entry.Date).Format(pushupDateLayout),
			Count:     entry.Count,
			UpdatedAt: entry.UpdatedAt.UTC().Format(time.RFC3339),
			UpdatedBy: entry.UpdatedBy,
		},
	})
}
