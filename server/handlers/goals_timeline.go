package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

type periodRange struct {
	Start time.Time
	End   time.Time
}

func parseTimelineParams(c *gin.Context) (*periodRange, string, error) {
	period := c.Query("period")
	if period == "" {
		return nil, "", nil
	}

	mode := c.DefaultQuery("mode", "planned")
	if mode != "planned" && mode != "achieved" && mode != "combined" {
		return nil, "", fmt.Errorf("mode must be one of: planned, achieved, combined")
	}

	value := c.Query("value")
	r, err := computePeriodRange(period, value)
	if err != nil {
		return nil, "", err
	}

	return r, mode, nil
}

func computePeriodRange(period, value string) (*periodRange, error) {
	now := time.Now().UTC()

	switch period {
	case "day":
		t, err := parseDayValue(value, now)
		if err != nil {
			return nil, err
		}
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 0, 1).Add(-time.Nanosecond)
		return &periodRange{Start: start, End: end}, nil

	case "week":
		start, err := parseWeekValue(value, now)
		if err != nil {
			return nil, err
		}
		end := start.AddDate(0, 0, 7).Add(-time.Nanosecond)
		return &periodRange{Start: start, End: end}, nil

	case "month":
		t, err := parseMonthValue(value, now)
		if err != nil {
			return nil, err
		}
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0).Add(-time.Nanosecond)
		return &periodRange{Start: start, End: end}, nil

	case "quarter":
		start, err := parseQuarterValue(value, now)
		if err != nil {
			return nil, err
		}
		end := start.AddDate(0, 3, 0).Add(-time.Nanosecond)
		return &periodRange{Start: start, End: end}, nil

	case "year":
		year, err := parseYearValue(value, now)
		if err != nil {
			return nil, err
		}
		start := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond)
		return &periodRange{Start: start, End: end}, nil

	case "custom":
		return parseCustomRange(value)

	default:
		return nil, fmt.Errorf("period must be one of: day, week, month, quarter, year, custom")
	}
}

// parseDayValue parses value as YYYY-MM-DD. If empty, uses today.
func parseDayValue(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return now, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("day value must be YYYY-MM-DD format")
	}
	return t, nil
}

// parseWeekValue parses ISO week value as YYYY-Www (e.g. "2026-W01").
// ISO 8601: weeks start on Monday.
func parseWeekValue(value string, now time.Time) (time.Time, error) {
	if value == "" {
		y, w := now.ISOWeek()
		return isoWeekStart(y, w), nil
	}
	parts := strings.Split(value, "-W")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("week value must be YYYY-Www format (e.g. 2026-W01)")
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 1 {
		return time.Time{}, fmt.Errorf("invalid year in week value")
	}
	week, err := strconv.Atoi(parts[1])
	if err != nil || week < 1 || week > 53 {
		return time.Time{}, fmt.Errorf("week number must be between 1 and 53")
	}
	start := isoWeekStart(year, week)
	// Validate that the computed week actually belongs to the requested year
	y, w := start.ISOWeek()
	if y != year || w != week {
		return time.Time{}, fmt.Errorf("week %d does not exist in year %d", week, year)
	}
	return start, nil
}

// isoWeekStart returns the Monday of ISO week (year, week).
func isoWeekStart(year, week int) time.Time {
	// Jan 4 is always in ISO week 1
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	// Find the Monday of the week containing Jan 4
	weekday := jan4.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	monday := jan4.AddDate(0, 0, -int(weekday-time.Monday))
	// Add (week-1) * 7 days
	return monday.AddDate(0, 0, (week-1)*7)
}

// parseMonthValue parses value as YYYY-MM. If empty, uses current month.
func parseMonthValue(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return now, nil
	}
	t, err := time.Parse("2006-01", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("month value must be YYYY-MM format")
	}
	return t, nil
}

// parseQuarterValue parses value as YYYY-Qn (e.g. "2026-Q1").
func parseQuarterValue(value string, now time.Time) (time.Time, error) {
	if value == "" {
		month := now.Month()
		q := (int(month) - 1) / 3
		startMonth := time.Month(q*3 + 1)
		return time.Date(now.Year(), startMonth, 1, 0, 0, 0, 0, time.UTC), nil
	}
	parts := strings.Split(value, "-Q")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("quarter value must be YYYY-Qn format (e.g. 2026-Q1)")
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 1 {
		return time.Time{}, fmt.Errorf("invalid year in quarter value")
	}
	q, err := strconv.Atoi(parts[1])
	if err != nil || q < 1 || q > 4 {
		return time.Time{}, fmt.Errorf("quarter must be between 1 and 4")
	}
	startMonth := time.Month((q-1)*3 + 1)
	return time.Date(year, startMonth, 1, 0, 0, 0, 0, time.UTC), nil
}

// parseYearValue parses value as YYYY. If empty, uses current year.
func parseYearValue(value string, now time.Time) (int, error) {
	if value == "" {
		return now.Year(), nil
	}
	year, err := strconv.Atoi(value)
	if err != nil || year < 1 {
		return 0, fmt.Errorf("year value must be a valid year (e.g. 2026)")
	}
	return year, nil
}

// parseCustomRange parses value as "YYYY-MM-DD..YYYY-MM-DD".
func parseCustomRange(value string) (*periodRange, error) {
	if value == "" {
		return nil, fmt.Errorf("custom period requires value in format YYYY-MM-DD..YYYY-MM-DD")
	}
	parts := strings.Split(value, "..")
	if len(parts) != 2 {
		return nil, fmt.Errorf("custom period value must be YYYY-MM-DD..YYYY-MM-DD")
	}
	start, err := time.Parse("2006-01-02", parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid start date: must be YYYY-MM-DD")
	}
	end, err := time.Parse("2006-01-02", parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid end date: must be YYYY-MM-DD")
	}
	if end.Before(start) {
		return nil, fmt.Errorf("end date must not be before start date")
	}
	endOfDay := time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 999999999, time.UTC)
	return &periodRange{Start: start, End: endOfDay}, nil
}

// applyTimelineFilter adds WHERE clauses to filter nodes by the timeline period and mode.
// planned: [planned_start, planned_end] overlaps [period.Start, period.End]
// achieved: completed_at within [period.Start, period.End]
// combined: both conditions must be true (AND)
func applyTimelineFilter(query *gorm.DB, r *periodRange, mode string) *gorm.DB {
	switch mode {
	case "planned":
		return applyPlannedFilter(query, r)
	case "achieved":
		return applyAchievedFilter(query, r)
	case "combined":
		return applyPlannedFilter(applyAchievedFilter(query, r), r)
	default:
		return query
	}
}

func applyPlannedFilter(query *gorm.DB, r *periodRange) *gorm.DB {
	// Overlap condition: node.planned_start <= period.End AND node.planned_end >= period.Start
	return query.Where(
		"planned_start IS NOT NULL AND planned_end IS NOT NULL AND planned_start <= ? AND planned_end >= ?",
		r.End, r.Start,
	)
}

func applyAchievedFilter(query *gorm.DB, r *periodRange) *gorm.DB {
	return query.Where("completed_at IS NOT NULL AND completed_at >= ? AND completed_at <= ?", r.Start, r.End)
}
