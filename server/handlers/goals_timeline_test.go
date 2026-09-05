package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func makeTime(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	t = t.UTC()
	return &t
}

// setupTimelineNodes creates a set of nodes with various timeline configurations.
func setupTimelineNodesDB(t *testing.T) (*httptest.ResponseRecorder, func(string) *httptest.ResponseRecorder) {
	t.Helper()
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Node 1: planned Jan 2026 only (not completed)
	n1 := data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Jan task",
		NodeType:     "step",
		SortOrder:    100,
		PlannedStart: makeTime("2026-01-05"),
		PlannedEnd:   makeTime("2026-01-20"),
	}
	db.Create(&n1)

	// Node 2: planned Feb 2026, completed in Feb
	completedFeb := makeTime("2026-02-15")
	n2 := data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Feb completed",
		NodeType:     "step",
		SortOrder:    200,
		PlannedStart: makeTime("2026-02-01"),
		PlannedEnd:   makeTime("2026-02-28"),
		CompletedAt:  completedFeb,
	}
	db.Create(&n2)

	// Node 3: planned spans Jan-Mar (Q1), completed in March
	completedMar := makeTime("2026-03-10")
	n3 := data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Q1 spanning",
		NodeType:     "milestone",
		SortOrder:    300,
		PlannedStart: makeTime("2026-01-15"),
		PlannedEnd:   makeTime("2026-03-31"),
		CompletedAt:  completedMar,
	}
	db.Create(&n3)

	// Node 4: no planned dates, completed in Jan
	completedJan := makeTime("2026-01-28")
	n4 := data_models.GoalNode{
		TreeID:      tree.ID,
		Name:        "Unplanned done",
		NodeType:    "step",
		SortOrder:   400,
		CompletedAt: completedJan,
	}
	db.Create(&n4)

	// Node 5: planned Dec 2025-Jan 2026, not completed (spans year boundary)
	n5 := data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Year boundary",
		NodeType:     "step",
		SortOrder:    500,
		PlannedStart: makeTime("2025-12-20"),
		PlannedEnd:   makeTime("2026-01-10"),
	}
	db.Create(&n5)

	// Node 6: planned in leap year date range (Feb 28-29, 2024)
	n6 := data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Leap year feb",
		NodeType:     "step",
		SortOrder:    600,
		PlannedStart: makeTime("2024-02-28"),
		PlannedEnd:   makeTime("2024-02-29"),
	}
	db.Create(&n6)

	// Node 7: planned week 53 of 2020 (ISO week 53 exists in 2020)
	// 2020-12-28 is Monday of ISO week 53
	n7 := data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Week 53 node",
		NodeType:     "step",
		SortOrder:    700,
		PlannedStart: makeTime("2020-12-28"),
		PlannedEnd:   makeTime("2021-01-03"),
	}
	db.Create(&n7)

	doReq := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		url := fmt.Sprintf("/goals/trees/%d/nodes%s", tree.ID, query)
		req, _ := http.NewRequest("GET", url, nil)
		router.ServeHTTP(w, req)
		return w
	}

	// Return a recorder for unfiltered list and the query function
	w := doReq("")
	return w, doReq
}

func TestTimeline_NoFilter_ReturnsAll(t *testing.T) {
	w, _ := setupTimelineNodesDB(t)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)
	if len(nodes) != 7 {
		t.Errorf("expected 7 nodes without filter, got %d", len(nodes))
	}
}

func TestTimeline_PlannedMonth_January(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=month&value=2026-01&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	// Should match: "Jan task" (fully in Jan), "Q1 spanning" (overlaps Jan), "Year boundary" (ends in Jan)
	names := nodeNames(nodes)
	assertContains(t, names, "Jan task")
	assertContains(t, names, "Q1 spanning")
	assertContains(t, names, "Year boundary")
	assertNotContains(t, names, "Feb completed")
	assertNotContains(t, names, "Unplanned done") // no planned dates
	assertNotContains(t, names, "Leap year feb")
}

func TestTimeline_AchievedMonth_January(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=month&value=2026-01&mode=achieved")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	// Should match: "Unplanned done" (completed Jan 28)
	names := nodeNames(nodes)
	assertContains(t, names, "Unplanned done")
	assertNotContains(t, names, "Jan task")      // not completed
	assertNotContains(t, names, "Feb completed") // completed in Feb
	assertNotContains(t, names, "Q1 spanning")   // completed in Mar
}

func TestTimeline_CombinedMode_PlannedAndAchieved(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	// Q1 2026: planned overlaps AND completed within
	w := doReq("?period=quarter&value=2026-Q1&mode=combined")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	// "Feb completed": planned in Feb (within Q1) AND completed in Feb (within Q1) ✓
	// "Q1 spanning": planned Jan-Mar (overlaps Q1) AND completed Mar 10 (within Q1) ✓
	// "Jan task": planned in Jan (within Q1) but NOT completed → ✗
	// "Unplanned done": no planned dates → ✗
	names := nodeNames(nodes)
	assertContains(t, names, "Feb completed")
	assertContains(t, names, "Q1 spanning")
	assertNotContains(t, names, "Jan task")
	assertNotContains(t, names, "Unplanned done")
}

func TestTimeline_ISOWeek53(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	// 2020 has ISO week 53 (starts Dec 28)
	w := doReq("?period=week&value=2020-W53&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	names := nodeNames(nodes)
	assertContains(t, names, "Week 53 node")
	if len(nodes) != 1 {
		t.Errorf("expected 1 node for week 53, got %d: %v", len(nodes), names)
	}
}

func TestTimeline_Week53_InvalidYear(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	// 2027 does NOT have week 53 (starts on Friday, only 52 weeks)
	w := doReq("?period=week&value=2027-W53&mode=planned")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-existent week 53, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTimeline_LeapYearFeb(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	// Feb 2024 has 29 days (leap year)
	w := doReq("?period=month&value=2024-02&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	names := nodeNames(nodes)
	assertContains(t, names, "Leap year feb")
	if len(nodes) != 1 {
		t.Errorf("expected 1 node for leap year feb, got %d: %v", len(nodes), names)
	}
}

func TestTimeline_CustomRange(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=custom&value=2026-01-01..2026-01-31&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	// Same as month=January planned filter
	names := nodeNames(nodes)
	assertContains(t, names, "Jan task")
	assertContains(t, names, "Q1 spanning")
	assertContains(t, names, "Year boundary")
	assertNotContains(t, names, "Feb completed")
}

func TestTimeline_CustomRange_EndBeforeStart(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=custom&value=2026-03-01..2026-01-01&mode=planned")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for end before start, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTimeline_InvalidPeriod(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=fortnight&mode=planned")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid period, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTimeline_InvalidMode(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=month&value=2026-01&mode=bogus")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid mode, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTimeline_DayFilter(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=day&value=2026-01-10&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	// Jan 10 is within: "Jan task" (5-20 Jan), "Year boundary" (20 Dec-10 Jan)
	// "Q1 spanning" starts Jan 15 so does NOT overlap Jan 10
	names := nodeNames(nodes)
	assertContains(t, names, "Jan task")
	assertContains(t, names, "Year boundary")
	assertNotContains(t, names, "Q1 spanning")
}

func TestTimeline_YearFilter(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=year&value=2024&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	names := nodeNames(nodes)
	assertContains(t, names, "Leap year feb")
	if len(nodes) != 1 {
		t.Errorf("expected 1 node for year 2024, got %d: %v", len(nodes), names)
	}
}

func TestTimeline_QuarterFilter(t *testing.T) {
	_, doReq := setupTimelineNodesDB(t)
	w := doReq("?period=quarter&value=2026-Q1&mode=planned")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)

	// Q1 2026 = Jan-Mar: "Jan task", "Feb completed", "Q1 spanning", "Year boundary"
	names := nodeNames(nodes)
	assertContains(t, names, "Jan task")
	assertContains(t, names, "Feb completed")
	assertContains(t, names, "Q1 spanning")
	assertContains(t, names, "Year boundary")
	assertNotContains(t, names, "Unplanned done")
	assertNotContains(t, names, "Leap year feb")
	assertNotContains(t, names, "Week 53 node")
}

func TestTimeline_PlannedButNotAchieved_CombinedQuery(t *testing.T) {
	db := setupTestDB(t)
	tree := createTestTree(db, 1)
	router := setupNodeRouter(db, 1)

	// Create a node that's planned in March but not completed
	db.Create(&data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Planned not done",
		NodeType:     "step",
		SortOrder:    100,
		PlannedStart: makeTime("2026-03-01"),
		PlannedEnd:   makeTime("2026-03-15"),
	})

	// Create a node that's planned in March AND completed in March
	completedMar := makeTime("2026-03-12")
	db.Create(&data_models.GoalNode{
		TreeID:       tree.ID,
		Name:         "Planned and done",
		NodeType:     "step",
		SortOrder:    200,
		PlannedStart: makeTime("2026-03-01"),
		PlannedEnd:   makeTime("2026-03-20"),
		CompletedAt:  completedMar,
	})

	// Combined filter for March: must have planned overlap AND completed in March
	w := httptest.NewRecorder()
	url := fmt.Sprintf("/goals/trees/%d/nodes?period=month&value=2026-03&mode=combined", tree.ID)
	req, _ := http.NewRequest("GET", url, nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var nodes []nodeResponse
	json.Unmarshal(w.Body.Bytes(), &nodes)
	names := nodeNames(nodes)

	// Only the one that's both planned and completed should appear
	assertContains(t, names, "Planned and done")
	assertNotContains(t, names, "Planned not done")
}

// --- Unit tests for period computation ---

func TestISOWeekStart_2020W53(t *testing.T) {
	// 2020 has 53 ISO weeks. Week 53 starts Mon Dec 28, 2020.
	start := isoWeekStart(2020, 53)
	expected := time.Date(2020, 12, 28, 0, 0, 0, 0, time.UTC)
	if !start.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, start)
	}
}

func TestISOWeekStart_2026W01(t *testing.T) {
	// 2026 W01 starts Mon Dec 29, 2025
	start := isoWeekStart(2026, 1)
	y, w := start.ISOWeek()
	if y != 2026 || w != 1 {
		t.Errorf("expected ISO week 2026-W01, got %d-W%02d", y, w)
	}
	if start.Weekday() != time.Monday {
		t.Errorf("expected Monday, got %s", start.Weekday())
	}
}

func TestComputePeriodRange_LeapYear_FebMonth(t *testing.T) {
	r, err := computePeriodRange("month", "2024-02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Feb 2024 has 29 days
	expectedEnd := time.Date(2024, 2, 29, 23, 59, 59, 999999999, time.UTC)
	if !r.End.Equal(expectedEnd) {
		t.Errorf("expected end %v, got %v", expectedEnd, r.End)
	}
}

func TestComputePeriodRange_NonLeapYear_FebMonth(t *testing.T) {
	r, err := computePeriodRange("month", "2025-02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Feb 2025 has 28 days
	expectedEnd := time.Date(2025, 2, 28, 23, 59, 59, 999999999, time.UTC)
	if !r.End.Equal(expectedEnd) {
		t.Errorf("expected end %v, got %v", expectedEnd, r.End)
	}
}

// --- Test helpers ---

func nodeNames(nodes []nodeResponse) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

func assertContains(t *testing.T, names []string, expected string) {
	t.Helper()
	for _, n := range names {
		if n == expected {
			return
		}
	}
	t.Errorf("expected %q to be in results %v", expected, names)
}

func assertNotContains(t *testing.T, names []string, unexpected string) {
	t.Helper()
	for _, n := range names {
		if n == unexpected {
			t.Errorf("expected %q NOT to be in results %v", unexpected, names)
			return
		}
	}
}
