package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"

	"aspirant-online/server/data_models"
)

// The audit trail is read a page at a time (system_3 #12270, the #11498
// review's C3-16).

type auditPageAnswer struct {
	Events     []data_models.InvoicingAuditEvent `json:"events"`
	NextCursor *string                           `json:"next_cursor"`
}

func getAuditPage(t *testing.T, r *gin.Engine, query string) auditPageAnswer {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/audit-events"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", query, w.Code, w.Body.String())
	}
	var a auditPageAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode %s: %v", query, err)
	}
	return a
}

// seedAuditTrail writes n events for owner, one second apart, except that
// when tie >= 0 events tie..tie+10 share one instant, so a page boundary can
// fall inside them. It returns the ids in the order the route serves:
// newest first, and by id, largest first, within one instant.
func seedAuditTrail(t *testing.T, db *gorm.DB, owner uint, n int, entityType string, entityID uint, tie int) []uint {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var events []data_models.InvoicingAuditEvent
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		if tie >= 0 && i > tie && i <= tie+10 {
			at = start.Add(time.Duration(tie) * time.Second)
		}
		e := data_models.InvoicingAuditEvent{CreatedAt: at, UserID: owner, Action: "update", EntityType: entityType, EntityID: entityID}
		if err := db.Create(&e).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].CreatedAt.After(events[j].CreatedAt)
		}
		return events[i].ID > events[j].ID
	})
	return eventIDs(events)
}

func eventIDs(es []data_models.InvoicingAuditEvent) []uint {
	out := make([]uint, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func sameIDs(a, b []uint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAuditEvents_PagesOf200NewestFirstToTheEnd(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	// 450 events; the boundary between the first and second page falls
	// inside eleven events of one instant (newest-first positions 194-204).
	want := seedAuditTrail(t, db, 42, 450, "client", 1, 245)
	seedAuditTrail(t, db, 7, 30, "client", 1, -1) // another book's

	var got []uint
	query := ""
	var sizes []int
	for i := 0; i < 5; i++ {
		page := getAuditPage(t, r, query)
		sizes = append(sizes, len(page.Events))
		got = append(got, eventIDs(page.Events)...)
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor != fmt.Sprint(page.Events[len(page.Events)-1].ID) {
			t.Fatalf("next_cursor %s is not the page's last event", *page.NextCursor)
		}
		query = "?cursor=" + *page.NextCursor
	}
	if fmt.Sprint(sizes) != "[200 200 50]" {
		t.Errorf("page sizes %v, want [200 200 50]", sizes)
	}
	if !sameIDs(got, want) {
		t.Errorf("the pages are not the trail newest first, each event once:\n got %v\nwant %v", got, want)
	}
}

func TestAuditEvents_LimitSetsThePageSize(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	want := seedAuditTrail(t, db, 42, 12, "client", 1, -1)

	page := getAuditPage(t, r, "?limit=5")
	if !sameIDs(eventIDs(page.Events), want[:5]) || page.NextCursor == nil {
		t.Fatalf("limit=5: %v %v", eventIDs(page.Events), page.NextCursor)
	}
	page = getAuditPage(t, r, "?limit=5&cursor="+*page.NextCursor)
	if !sameIDs(eventIDs(page.Events), want[5:10]) || page.NextCursor == nil {
		t.Fatalf("second page: %v", eventIDs(page.Events))
	}
	page = getAuditPage(t, r, "?limit=5&cursor="+*page.NextCursor)
	if !sameIDs(eventIDs(page.Events), want[10:]) || page.NextCursor != nil {
		t.Fatalf("last page: %v %v", eventIDs(page.Events), page.NextCursor)
	}
	// A page that holds exactly the rest has no next page.
	if page := getAuditPage(t, r, "?limit=12"); len(page.Events) != 12 || page.NextCursor != nil {
		t.Fatalf("limit=12 of 12: %d %v", len(page.Events), page.NextCursor)
	}
	if page := getAuditPage(t, r, "?limit=1000"); len(page.Events) != 12 {
		t.Fatalf("limit=1000: %d", len(page.Events))
	}
	for _, bad := range []string{"0", "-1", "1001", "x", "", "2.5"} {
		w := invoicingDo(r, "GET", "/invoicing/audit-events?limit="+bad, "")
		if w.Code != http.StatusBadRequest || errorCode(t, w) != "invalid_limit" {
			t.Errorf("limit=%q: %d %s", bad, w.Code, w.Body.String())
		}
	}
}

// A read narrowed to one record takes no default limit, so a History fold
// never loses the record's oldest line (the review's point 3).
func TestAuditEvents_ARecordsHistoryComesWhole(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	want := seedAuditTrail(t, db, 42, 260, "payment", 9, -1)
	seedAuditTrail(t, db, 42, 5, "payment", 10, -1)

	page := getAuditPage(t, r, "?entity_type=payment&entity_id=9")
	if !sameIDs(eventIDs(page.Events), want) || page.NextCursor != nil {
		t.Fatalf("one record's 260 events: got %d, next %v", len(page.Events), page.NextCursor)
	}
	// It may still ask for a page.
	if page := getAuditPage(t, r, "?entity_type=payment&entity_id=9&limit=100"); len(page.Events) != 100 || page.NextCursor == nil {
		t.Fatalf("limit=100 on one record: %d %v", len(page.Events), page.NextCursor)
	}
	// Narrowed to a kind of record only, the default applies.
	if page := getAuditPage(t, r, "?entity_type=payment"); len(page.Events) != 200 || page.NextCursor == nil {
		t.Fatalf("one kind of record: %d %v", len(page.Events), page.NextCursor)
	}
}

func TestAuditEvents_ACursorNotFromThisBookIsRefusedAsUnknown(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	seedAuditTrail(t, db, 42, 3, "client", 1, -1)
	other := seedAuditTrail(t, db, 7, 3, "client", 1, -1)

	answer := func(cursor string) string {
		w := invoicingDo(r, "GET", "/invoicing/audit-events?cursor="+cursor, "")
		if w.Code != http.StatusBadRequest || errorCode(t, w) != "invalid_cursor" {
			t.Errorf("cursor=%q: %d %s", cursor, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	unknown := answer("999999")
	if got := answer(fmt.Sprint(other[0])); got != unknown {
		t.Errorf("another book's event answers differently from an unknown one:\n%s\n%s", got, unknown)
	}
	for _, bad := range []string{"0", "x", "", "-3", "2147483648", "9223372036854775808"} {
		answer(bad)
	}
}

// The cursor's condition stays inside the owner filter: another book's
// event at the cursor's exact instant, with a smaller id, would match the
// cursor's "as old with a smaller id" were the OR outside it (security's
// ask on PR 245, c76613 on system_3 #12270).
func TestAuditEvents_TheCursorNeverReachesAnotherBook(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	add := func(owner uint, when time.Time) uint {
		e := data_models.InvoicingAuditEvent{CreatedAt: when, UserID: owner, Action: "update", EntityType: "client", EntityID: 1}
		if err := db.Create(&e).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		return e.ID
	}
	other := add(7, at) // first, so its id is below the cursor's
	older := add(42, at.Add(-10*time.Second))
	cursor := add(42, at)
	newer := add(42, at.Add(10*time.Second))
	if other >= cursor {
		t.Fatalf("the other book's event %d is not below the cursor %d", other, cursor)
	}

	page := getAuditPage(t, r, "?limit=2")
	if !sameIDs(eventIDs(page.Events), []uint{newer, cursor}) || page.NextCursor == nil || *page.NextCursor != fmt.Sprint(cursor) {
		t.Fatalf("first page: %v next %v", eventIDs(page.Events), page.NextCursor)
	}
	page = getAuditPage(t, r, "?limit=2&cursor="+*page.NextCursor)
	if !sameIDs(eventIDs(page.Events), []uint{older}) || page.NextCursor != nil {
		t.Errorf("the page after the cursor: %v next %v, want only %d (never the other book's %d)",
			eventIDs(page.Events), page.NextCursor, older, other)
	}
}
