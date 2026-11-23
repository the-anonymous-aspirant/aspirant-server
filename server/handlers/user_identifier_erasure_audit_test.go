package handlers

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// Thirty days after deletion an account's invoicing audit rows keep who did
// what, and when, but not the names and addresses their details copied
// (system_3 #12081; the operator's answer c74952 on #11380). Every value here
// is invented.

// seedAuditRows gives userID one audit row per kind of details a writer
// leaves: a client snapshot, a payee name, and none at all.
func seedAuditRows(t *testing.T, db *gorm.DB, userID uint, n int) []data_models.InvoicingAuditEvent {
	t.Helper()
	rows := []data_models.InvoicingAuditEvent{
		{CreatedAt: daysAgo(40), UserID: userID, Action: "create", EntityType: "client", EntityID: uint(100 + n),
			Details: fmt.Sprintf(`{"after":{"name":"Invented Client %d","email":"client%d@example.test","address":"1 Invented Way"}}`, n, n)},
		{CreatedAt: daysAgo(39), UserID: userID, Action: "edit", EntityType: "payment", EntityID: uint(200 + n),
			Details: fmt.Sprintf(`{"before":{"sender":"Invented Sender %d"},"after":{"sender":"Invented Sender %d B"}}`, n, n)},
		{CreatedAt: daysAgo(38), UserID: userID, Action: "delete", EntityType: "invoice", EntityID: uint(300 + n), Details: ""},
	}
	for i := range rows {
		mustCreate(t, db, &rows[i])
	}
	return rows
}

func readAuditRows(t *testing.T, db *gorm.DB, userID uint) []data_models.InvoicingAuditEvent {
	t.Helper()
	var rows []data_models.InvoicingAuditEvent
	if err := db.Where("user_id = ?", userID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read audit rows of %d: %v", userID, err)
	}
	return rows
}

// assertEmptied checks that each row kept everything but its details, and
// that details holding something became "{}" while "" stayed "".
func assertEmptied(t *testing.T, got, seeded []data_models.InvoicingAuditEvent) {
	t.Helper()
	if len(got) != len(seeded) {
		t.Fatalf("rows: got %d, want %d (none may be deleted)", len(got), len(seeded))
	}
	for i, r := range got {
		s := seeded[i]
		if r.ID != s.ID || r.UserID != s.UserID || r.Action != s.Action || r.EntityType != s.EntityType ||
			r.EntityID != s.EntityID || !r.CreatedAt.Equal(s.CreatedAt) {
			t.Errorf("row %d: the trail changed: got %+v, seeded %+v", s.ID, r, s)
		}
		want := "{}"
		if s.Details == "" {
			want = ""
		}
		if r.Details != want {
			t.Errorf("row %d (%s %s): details %q, want %q", s.ID, s.Action, s.EntityType, r.Details, want)
		}
	}
}

func assertUntouched(t *testing.T, got, seeded []data_models.InvoicingAuditEvent) {
	t.Helper()
	if len(got) != len(seeded) {
		t.Fatalf("rows: got %d, want %d", len(got), len(seeded))
	}
	for i := range got {
		if got[i].Details != seeded[i].Details {
			t.Errorf("row %d: details %q, want them kept as %q", seeded[i].ID, got[i].Details, seeded[i].Details)
		}
	}
}

func TestErasureAudit_ADueAccountsDetailsAreEmptiedAndItsTrailKept(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	d31 := daysAgo(31)
	due, _ := seedErasureUser(t, db, store, 1, &d31, "")
	live, _ := seedErasureUser(t, db, store, 2, nil, "")
	dueRows := seedAuditRows(t, db, due.ID, 1)
	liveRows := seedAuditRows(t, db, live.ID, 2)

	if n := eraseDueIdentifiers(db, store, erasureNow); n != 1 {
		t.Fatalf("erased %d accounts, want 1", n)
	}
	assertEmptied(t, readAuditRows(t, db, due.ID), dueRows)
	assertUntouched(t, readAuditRows(t, db, live.ID), liveRows)
}

func TestErasureAudit_A29DaysDeletedAccountKeepsItsDetails(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	d29 := daysAgo(29)
	recent, _ := seedErasureUser(t, db, store, 1, &d29, "")
	rows := seedAuditRows(t, db, recent.ID, 1)

	if n := eraseDueIdentifiers(db, store, erasureNow); n != 0 {
		t.Fatalf("erased %d accounts, want 0", n)
	}
	assertUntouched(t, readAuditRows(t, db, recent.ID), rows)
}

func TestErasureAudit_ASecondPassChangesNothingAndLogsNothing(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	d31 := daysAgo(31)
	due, _ := seedErasureUser(t, db, store, 1, &d31, "")
	seedAuditRows(t, db, due.ID, 1)
	eraseDueIdentifiers(db, store, erasureNow)
	after := readAuditRows(t, db, due.ID)

	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	if n := eraseDueIdentifiers(db, store, erasureNow.Add(1)); n != 0 {
		t.Fatalf("second pass erased %d accounts, want 0", n)
	}
	assertUntouched(t, readAuditRows(t, db, due.ID), after)
	if out.Len() != 0 {
		t.Errorf("second pass logged %q, want nothing", out.String())
	}
	if n, err := data_models.EmptyErasedAccountsAuditDetails(db); err != nil || n != 0 {
		t.Errorf("catch-up after the erase: changed %d rows (err %v), want 0", n, err)
	}
}

// An account the sweep erased before its audit rows were part of the erase
// (the boot pass of 2026-11-18, c72253 on #11380) has them emptied by the
// next pass, and a live account's and a recent delete's rows stay.
func TestErasureAudit_AnAccountErasedEarlierHasItsDetailsEmptiedByTheNextPass(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	d60, d29 := daysAgo(60), daysAgo(29)
	earlier, _ := seedErasureUser(t, db, store, 1, &d60, "")
	recent, _ := seedErasureUser(t, db, store, 2, &d29, "")
	live, _ := seedErasureUser(t, db, store, 3, nil, "")
	// Erased as main's sweep did it: the user row only.
	if err := db.Unscoped().Model(&data_models.User{}).Where("id = ?", earlier.ID).UpdateColumns(map[string]interface{}{
		"Username": data_models.ErasedUsername(earlier.ID), "Email": "", "Password": "", "Comment": "", "IdentifiersErasedAt": daysAgo(30),
	}).Error; err != nil {
		t.Fatalf("erase as before: %v", err)
	}
	earlierRows := seedAuditRows(t, db, earlier.ID, 1)
	recentRows := seedAuditRows(t, db, recent.ID, 2)
	liveRows := seedAuditRows(t, db, live.ID, 3)

	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	if n := eraseDueIdentifiers(db, store, erasureNow); n != 0 {
		t.Fatalf("erased %d accounts, want 0 (the earlier one is erased already)", n)
	}
	assertEmptied(t, readAuditRows(t, db, earlier.ID), earlierRows)
	assertUntouched(t, readAuditRows(t, db, recent.ID), recentRows)
	assertUntouched(t, readAuditRows(t, db, live.ID), liveRows)

	logged := out.String()
	if !strings.Contains(logged, "emptied the details of 2 invoicing audit row(s) of erased accounts") {
		t.Errorf("log %q: want the count of rows emptied", logged)
	}
	for _, s := range []string{"Invented", "example.test", "Invented Way"} {
		if strings.Contains(logged, s) {
			t.Errorf("log carries %q: %q", s, logged)
		}
	}
}

// The erase and the emptying commit together: when the audit update fails,
// the account is not erased either, and the next pass retries both.
func TestErasureAudit_AFailedEmptyingLeavesTheAccountForTheNextPass(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	d31 := daysAgo(31)
	due, _ := seedErasureUser(t, db, store, 1, &d31, "")
	if err := db.DropTable(&data_models.InvoicingAuditEvent{}).Error; err != nil {
		t.Fatalf("drop audit table: %v", err)
	}

	log.SetOutput(&bytes.Buffer{})
	defer log.SetOutput(os.Stderr)
	if n := eraseDueIdentifiers(db, store, erasureNow); n != 0 {
		t.Fatalf("erased %d accounts with the audit update failing, want 0", n)
	}
	if got := readErasureUser(t, db, due.ID); got.IdentifiersErasedAt != nil || got.Username != due.Username {
		t.Fatalf("the account was erased though its audit rows were not: %+v", got)
	}

	db.AutoMigrate(&data_models.InvoicingAuditEvent{})
	if n := eraseDueIdentifiers(db, store, erasureNow); n != 1 {
		t.Fatalf("next pass erased %d accounts, want 1", n)
	}
}

// An admin's download of a user's failed import is audited under the admin's
// id, with the user's id and the file's name in details (security c75300 on
// #12081). The erase empties it by owner_user_id (E2), and a malformed row
// next to it neither stalls the erase nor is touched (E1).
func seedDownloadRow(t *testing.T, db *gorm.DB, adminID uint, details string) data_models.InvoicingAuditEvent {
	t.Helper()
	r := data_models.InvoicingAuditEvent{CreatedAt: daysAgo(35), UserID: adminID, Action: invoicingActionDownload,
		EntityType: invoicingEntityImportFailure, EntityID: 77, Details: details}
	mustCreate(t, db, &r)
	return r
}

func readAuditRow(t *testing.T, db *gorm.DB, id uint) data_models.InvoicingAuditEvent {
	t.Helper()
	var r data_models.InvoicingAuditEvent
	if err := db.First(&r, id).Error; err != nil {
		t.Fatalf("read audit row %d: %v", id, err)
	}
	return r
}

func TestErasureAudit_AnAdminsDownloadRowAboutTheAccountIsEmptiedByOwnerID(t *testing.T) {
	for _, erasedBefore := range []bool{false, true} {
		t.Run(fmt.Sprintf("erased-before-this-shipped=%v", erasedBefore), func(t *testing.T) {
			db := setupDeleteContentDB(t)
			store := newErasureStore(t)
			d31 := daysAgo(31)
			due, _ := seedErasureUser(t, db, store, 1, &d31, "")
			other, _ := seedErasureUser(t, db, store, 2, nil, "")
			admin, _ := seedErasureUser(t, db, store, 3, nil, "")
			if erasedBefore {
				// Erased as main's sweep did it: the user row only.
				if err := db.Unscoped().Model(&data_models.User{}).Where("id = ?", due.ID).
					UpdateColumns(map[string]interface{}{"Email": "", "IdentifiersErasedAt": daysAgo(1)}).Error; err != nil {
					t.Fatalf("erase as before: %v", err)
				}
			}
			about := seedDownloadRow(t, db, admin.ID, fmt.Sprintf(`{"filename":"invented-statement-person1.csv","owner_user_id":%d}`, due.ID))
			aboutOther := seedDownloadRow(t, db, admin.ID, fmt.Sprintf(`{"filename":"invented-statement-person2.csv","owner_user_id":%d}`, other.ID))
			// E2: the same file name, another owner. Matching by name would take it.
			sameName := seedDownloadRow(t, db, admin.ID, fmt.Sprintf(`{"filename":"invented-statement-person1.csv","owner_user_id":%d}`, other.ID))
			empty := seedDownloadRow(t, db, admin.ID, "")
			notJSON := seedDownloadRow(t, db, admin.ID, fmt.Sprintf(`{"owner_user_id":%d, not json`, due.ID))
			// The same id written as a string: `->>` reads it as the same text,
			// and so does this match. It is the account's row, so it goes too.
			asString := seedDownloadRow(t, db, admin.ID, fmt.Sprintf(`{"owner_user_id":"%d","filename":"invented-x.csv"}`, due.ID))

			log.SetOutput(&bytes.Buffer{})
			defer log.SetOutput(os.Stderr)
			n := eraseDueIdentifiers(db, store, erasureNow)
			if want := map[bool]int{false: 1, true: 0}[erasedBefore]; n != want {
				t.Fatalf("erased %d accounts, want %d", n, want)
			}
			if got := readErasureUser(t, db, due.ID); got.IdentifiersErasedAt == nil {
				t.Fatalf("the account was not erased")
			}
			for _, r := range []data_models.InvoicingAuditEvent{about, asString} {
				got := readAuditRow(t, db, r.ID)
				if got.Details != "{}" || got.UserID != admin.ID || got.Action != r.Action ||
					got.EntityType != r.EntityType || got.EntityID != r.EntityID || !got.CreatedAt.Equal(r.CreatedAt) {
					t.Errorf("download row %d about the account: got %+v, want details {} and the trail kept", r.ID, got)
				}
			}
			for _, r := range []data_models.InvoicingAuditEvent{aboutOther, sameName, empty, notJSON} {
				if got := readAuditRow(t, db, r.ID).Details; got != r.Details {
					t.Errorf("row %d: details %q, want them kept as %q", r.ID, got, r.Details)
				}
			}
			if n, err := data_models.EmptyErasedAccountsAuditDetails(db); err != nil || n != 0 {
				t.Errorf("a further pass changed %d rows (err %v), want 0", n, err)
			}
		})
	}
}
