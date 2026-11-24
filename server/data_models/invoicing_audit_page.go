package data_models

import (
	"math"

	"github.com/jinzhu/gorm"
)

// The audit trail is read a page at a time (system_3 #12270, the #11498
// review's C3-16): the whole trail came back in one answer, 7.3 MB on a
// probe book. A read of the whole book, or of one kind of record, takes a
// page of DefaultAuditPageSize unless it asks for another size. A read
// narrowed to one record takes no default (the review's point 3): the
// History folds draw every line they are sent and have no "more", so the
// first line a cut would lose is the record's oldest, "Created" or
// "Imported from ...".
const (
	DefaultAuditPageSize = 200
	MaxAuditPageSize     = 1000
)

// AuditEventsPage is one page of the trail, newest first. NextCursor is the
// id of the page's last event when older events remain, else 0.
type AuditEventsPage struct {
	Events     []InvoicingAuditEvent
	NextCursor uint
}

// ListInvoicingAuditEventsPage reads the owner's events newest first, with
// ListInvoicingAuditEvents' narrowing. limit 0 reads every event. cursor,
// when not 0, is an event id from an earlier page's NextCursor: the page
// starts with the event after it. found is false when the cursor names no
// event of the owner's, which the caller answers as a bad request; an event
// of another book reads exactly as an unknown one.
func ListInvoicingAuditEventsPage(db *gorm.DB, owner uint, entityType string, entityID uint, limit int, cursor uint) (page AuditEventsPage, found bool, err error) {
	page.Events = []InvoicingAuditEvent{}
	// Postgres's integer columns hold ids up to MaxInt32; a larger id names
	// no row, and is never sent to the database (as #12268 does for every
	// invoicing id).
	if entityID > math.MaxInt32 {
		return page, true, nil
	}
	q := db.Where("user_id = ?", owner).Order("created_at desc, id desc")
	if entityType != "" {
		q = q.Where("entity_type = ?", entityType)
	}
	if entityID > 0 {
		q = q.Where("entity_id = ?", entityID)
	}
	if cursor > 0 {
		if cursor > math.MaxInt32 {
			return page, false, nil
		}
		var at InvoicingAuditEvent
		res := db.Where("id = ? AND user_id = ?", cursor, owner).First(&at)
		if res.RecordNotFound() {
			return page, false, nil
		}
		if res.Error != nil {
			return page, false, res.Error
		}
		// After the cursor in the order above: older, or as old with a
		// smaller id. Two events of one instant are never split or repeated.
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", at.CreatedAt, at.CreatedAt, at.ID)
	}
	if limit > 0 {
		// One past the page tells whether older events remain.
		q = q.Limit(limit + 1)
	}
	if err := q.Find(&page.Events).Error; err != nil {
		return AuditEventsPage{Events: []InvoicingAuditEvent{}}, true, err
	}
	if limit > 0 && len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.NextCursor = page.Events[limit-1].ID
	}
	return page, true, nil
}
