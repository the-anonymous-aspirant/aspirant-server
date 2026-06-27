package handlers

import (
	"log"
	"net/http"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Challenge window: 60 daily rows from start (inclusive) to end (inclusive).
var (
	pushupChallengeStart = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	pushupChallengeEnd   = time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
)

// pushupEditWindowDays is the symmetric span around current_date that the
// edit-window rule allows: the row for today, the 2 preceding, and the 2
// following — a 5-row sliding window.
const pushupEditWindowDays = 2

// pushupDateLayout matches what the :date URL param and JSON payload use.
const pushupDateLayout = "2006-01-02"

// truncateToDay drops time-of-day to UTC midnight for consistent date PKs.
func truncateToDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func inPushupChallengeWindow(date time.Time) bool {
	d := truncateToDay(date)
	return !d.Before(pushupChallengeStart) && !d.After(pushupChallengeEnd)
}

func inPushupEditWindow(date time.Time, now time.Time) bool {
	d := truncateToDay(date)
	today := truncateToDay(now)
	diff := int(d.Sub(today).Hours() / 24)
	if diff < 0 {
		diff = -diff
	}
	return diff <= pushupEditWindowDays
}

type pushupEntryDTO struct {
	Date      string `json:"date"`
	Count     *int   `json:"count"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy uint   `json:"updated_by,omitempty"`
}

// gapFillEntries returns one DTO per day in the challenge window, populated
// from saved rows when present and otherwise as a null-count placeholder.
// The frontend table layout assumes a complete 60-row response.
func gapFillEntries(saved []data_models.PushupEntry) []pushupEntryDTO {
	byDate := make(map[string]data_models.PushupEntry, len(saved))
	for _, e := range saved {
		byDate[truncateToDay(e.Date).Format(pushupDateLayout)] = e
	}
	out := make([]pushupEntryDTO, 0, 60)
	for d := pushupChallengeStart; !d.After(pushupChallengeEnd); d = d.AddDate(0, 0, 1) {
		key := d.Format(pushupDateLayout)
		if e, ok := byDate[key]; ok {
			out = append(out, pushupEntryDTO{
				Date:      key,
				Count:     e.Count,
				UpdatedAt: e.UpdatedAt.UTC().Format(time.RFC3339),
				UpdatedBy: e.UpdatedBy,
			})
			continue
		}
		out = append(out, pushupEntryDTO{Date: key, Count: nil})
	}
	return out
}

// GetPushupEntriesHandler returns all 60 days, gap-filled.
func GetPushupEntriesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	saved, err := data_models.GetPushupEntries(db)
	if err != nil {
		log.Printf("Error retrieving pushup entries: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving pushup entries")
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": gapFillEntries(saved)})
}

type patchPushupEntryRequest struct {
	Count *int `json:"count"`
}

// PatchPushupEntryHandler upserts a single date. The edit-window rule is
// enforced server-side as the source of truth — the frontend mirrors it
// for UX, but the server is the gate.
func PatchPushupEntryHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	dateStr := c.Param("date")
	date, err := time.Parse(pushupDateLayout, dateStr)
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid date format, expected YYYY-MM-DD")
		return
	}
	date = truncateToDay(date)

	if !inPushupChallengeWindow(date) {
		RespondWithError(c, http.StatusBadRequest, "Date outside challenge window (2026-07-01..2026-08-29)")
		return
	}
	if !inPushupEditWindow(date, time.Now()) {
		RespondWithError(c, http.StatusForbidden, "Date outside edit window (current_date ± 2 days)")
		return
	}

	var req patchPushupEntryRequest
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

	entry, err := data_models.UpsertPushupEntry(db, date, req.Count, userID)
	if err != nil {
		log.Printf("Error upserting pushup entry: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving pushup entry")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"entry": pushupEntryDTO{
			Date:      truncateToDay(entry.Date).Format(pushupDateLayout),
			Count:     entry.Count,
			UpdatedAt: entry.UpdatedAt.UTC().Format(time.RFC3339),
			UpdatedBy: entry.UpdatedBy,
		},
	})
}

// GetPushupMilestonesHandler returns the seeded cumulative→message map.
func GetPushupMilestonesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	milestones, err := data_models.GetPushupMilestones(db)
	if err != nil {
		log.Printf("Error retrieving pushup milestones: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving pushup milestones")
		return
	}
	c.JSON(http.StatusOK, gin.H{"milestones": milestones})
}
