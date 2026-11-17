package handlers

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// A user who still has invoicing records cannot be deleted: the request is
// refused with 409 and nothing at all is removed (system_3 #11590, Part A of
// #11471; the operator's answer c70521 on #11380). Every value is invented.

// userInvoicingModels is every invoicing table a delete may read. Any harness
// that deletes a user migrates them through userContentModels, as a real
// database has them: a missing table is an error that rolls the delete back.
func userInvoicingModels() []interface{} {
	return []interface{}{
		&data_models.InvoicingProfile{}, &data_models.InvoicingProfileLogo{}, &data_models.InvoicingClient{},
		&data_models.InvoicingInvoice{}, &data_models.InvoicingPayment{}, &data_models.InvoicingPaymentTombstone{},
		&data_models.InvoicingExpense{}, &data_models.InvoicingExpenseTombstone{}, &data_models.InvoicingExpenseCategory{},
		&data_models.InvoicingExpenseReceipt{}, &data_models.InvoicingImportFailure{},
		&data_models.InvoicingBookSelection{}, &data_models.InvoicingBookPreset{}, &data_models.InvoicingAuditEvent{},
	}
}

var guardDay = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

// blockingTables is this file's own list of the tables that refuse a delete,
// each with one minimal row owned by `owner`. It is written out here and not
// read from the code under test, so a table dropped from the check fails by
// name in TestDeleteUser_RefusedForARowInAnySingleInvoicingTable, and a table
// added to the check without a row here fails in the list comparison below.
var blockingTables = []struct {
	name string
	row  func(owner uint) interface{}
}{
	{"InvoicingProfile", func(owner uint) interface{} {
		return &data_models.InvoicingProfile{OwnerUserID: owner, FullName: "Invented Person"}
	}},
	{"InvoicingProfileLogo", func(owner uint) interface{} {
		return &data_models.InvoicingProfileLogo{OwnerUserID: owner, Content: []byte("invented"), SHA256: "invented", Width: 1, Height: 1}
	}},
	{"InvoicingClient", func(owner uint) interface{} {
		return &data_models.InvoicingClient{OwnerUserID: owner, Name: "Invented Client"}
	}},
	{"InvoicingInvoice", func(owner uint) interface{} {
		return &data_models.InvoicingInvoice{OwnerUserID: owner, Status: "draft"}
	}},
	{"InvoicingPayment", func(owner uint) interface{} {
		return &data_models.InvoicingPayment{OwnerUserID: owner, Source: "manual", TransactionID: fmt.Sprintf("M-invented-%d", owner), Kind: "incoming", PaidOn: guardDay, Currency: "EUR", GrossCents: 1000}
	}},
	{"InvoicingExpense", func(owner uint) interface{} {
		return &data_models.InvoicingExpense{OwnerUserID: owner, Source: "manual", TransactionID: fmt.Sprintf("E-invented-%d", owner), PaidOn: guardDay, Currency: "EUR", AmountCents: 500}
	}},
	{"InvoicingExpenseReceipt", func(owner uint) interface{} {
		return &data_models.InvoicingExpenseReceipt{ExpenseID: 900 + owner, OwnerUserID: owner, Content: []byte("invented"), ContentType: "application/pdf", Filename: "invented.pdf", SHA256: "invented", SizeBytes: 8}
	}},
	{"InvoicingImportFailure", func(owner uint) interface{} {
		return &data_models.InvoicingImportFailure{OwnerUserID: owner, ImportKind: "n26", Filename: "invented.pdf"}
	}},
	{"InvoicingBookSelection", func(owner uint) interface{} {
		return &data_models.InvoicingBookSelection{OwnerUserID: owner, Preset: "everything"}
	}},
	{"InvoicingBookPreset", func(owner uint) interface{} {
		return &data_models.InvoicingBookPreset{OwnerUserID: owner, Name: "Invented preset"}
	}},
}

// remnantTables never refuse a delete and are removed with the account
// (security's ruling c71589 on system_3 #11471): seeded expense categories and
// the tombstones a hard delete of a payment or expense leaves behind.
var remnantTables = []struct {
	name string
	row  func(owner uint) interface{}
}{
	{"InvoicingExpenseCategory", func(owner uint) interface{} {
		return &data_models.InvoicingExpenseCategory{OwnerUserID: owner, Name: "invented category", CountsAs: "other"}
	}},
	{"InvoicingPaymentTombstone", func(owner uint) interface{} {
		return &data_models.InvoicingPaymentTombstone{OwnerUserID: owner, Source: "manual", TransactionID: fmt.Sprintf("M-gone-%d", owner), DeletedOn: guardDay, DeletedBy: owner}
	}},
	{"InvoicingExpenseTombstone", func(owner uint) interface{} {
		return &data_models.InvoicingExpenseTombstone{OwnerUserID: owner, Source: "manual", TransactionID: fmt.Sprintf("E-gone-%d", owner), DeletedOn: guardDay, DeletedBy: owner}
	}},
}

// auditRow is an audit event written under `actor`. Audit rows never refuse a
// delete and are never removed by one.
func auditRow(actor uint, action, entity, details string) *data_models.InvoicingAuditEvent {
	return &data_models.InvoicingAuditEvent{UserID: actor, Action: action, EntityType: entity, EntityID: 7, Details: details}
}

const refusedMessage = "This user still has invoicing records, which must be exported and removed before the account can be deleted"

// guardScene is two users with personal content and a shared room, as in the
// #11384 test, and a router that deletes through the real handler.
type guardScene struct {
	db         *gorm.DB
	gone, kept data_models.User
	room       data_models.Room
	router     *gin.Engine
}

func newGuardScene(t *testing.T) guardScene {
	t.Helper()
	db := setupDeleteContentDB(t)
	role := data_models.Role{RoleName: "Member"}
	mustCreate(t, db, &role)
	gone := data_models.User{Username: "invented-gone", Email: "gone@example.test", RoleID: role.ID}
	mustCreate(t, db, &gone)
	kept := data_models.User{Username: "invented-kept", Email: "kept@example.test", RoleID: role.ID}
	mustCreate(t, db, &kept)
	seedPersonal(t, db, gone, 1)
	seedPersonal(t, db, kept, 2)
	room := data_models.Room{Code: "INV09", PlayerCount: 2, Status: "active", CreatorUserID: gone.ID}
	mustCreate(t, db, &room)
	mustCreate(t, db, &data_models.RoomMember{RoomID: room.ID, UserID: gone.ID, Slot: 1})
	mustCreate(t, db, &data_models.RoomMember{RoomID: room.ID, UserID: kept.ID, Slot: 2})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", uint(999)) // an admin who is neither user
		c.Next()
	})
	r.DELETE("/users/:id", DeleteUserHandler)
	return guardScene{db: db, gone: gone, kept: kept, room: room, router: r}
}

// contentCounts is the group 1 and group 2 rows of one user, by table, counted
// Unscoped. It is what a refused delete must leave exactly as it was.
func contentCounts(t *testing.T, db *gorm.DB, u data_models.User) map[string]int {
	t.Helper()
	out := map[string]int{}
	for name, model := range map[string]interface{}{
		"scratchpads": &data_models.Scratchpad{}, "goal_trees": &data_models.GoalTree{},
		"user_display_names": &data_models.UserDisplayName{}, "user_tokens": &data_models.UserToken{},
		"constellation_profiles": &data_models.ConstellationProfile{}, "player_goals": &data_models.PlayerGoal{},
		"easter_hunt_clicks": &data_models.EasterHuntClick{}, "easter_hunt_scores": &data_models.EasterHuntScore{},
		"constellation_room_members": &data_models.RoomMember{},
	} {
		out[name] = count(t, db, model, "user_id = ?", u.ID)
	}
	out["game_scores"] = count(t, db, &data_models.GameScore{}, "user_id = ?", int(u.ID))
	out["rooms_created"] = count(t, db, &data_models.Room{}, "creator_user_id = ?", u.ID)
	return out
}

func isLive(t *testing.T, db *gorm.DB, id uint) bool {
	t.Helper()
	var u data_models.User
	if err := db.Unscoped().First(&u, id).Error; err != nil {
		t.Fatalf("read user %d: %v", id, err)
	}
	return u.DeletedAt == nil
}

// The two lists in the code are exactly the two lists here. A refusing table
// dropped from the check, a non-refusing one counted by it, or a new table in
// either list with no row in this file, fails here by name.
func TestDeleteUser_TheCheckReadsExactlyTheTablesThisFileSeeds(t *testing.T) {
	want := make([]string, 0, len(blockingTables))
	for _, bt := range blockingTables {
		want = append(want, bt.name)
	}
	if len(want) != 10 {
		t.Fatalf("this file lists %d refusing tables, want the ten of the ruling", len(want))
	}
	if got := data_models.UserInvoicingTableNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("the delete check reads %v, and this file seeds %v", got, want)
	}
	wantRemnants := make([]string, 0, len(remnantTables))
	for _, rt := range remnantTables {
		wantRemnants = append(wantRemnants, rt.name)
	}
	if got := data_models.UserInvoicingRemnantTableNames(); !reflect.DeepEqual(got, wantRemnants) {
		t.Errorf("the delete removes %v, and this file seeds %v", got, wantRemnants)
	}
}

func TestDeleteUser_RefusedForARowInAnySingleInvoicingTable(t *testing.T) {
	for _, bt := range blockingTables {
		t.Run(bt.name, func(t *testing.T) {
			s := newGuardScene(t)
			mustCreate(t, s.db, bt.row(s.gone.ID))
			before := contentCounts(t, s.db, s.gone)
			// Positive control: the user has content a delete would remove.
			if before["scratchpads"] == 0 || before["constellation_room_members"] == 0 {
				t.Fatalf("seed: the user has nothing for a delete to remove: %v", before)
			}

			w := fvDo(t, s.router, "DELETE", fmt.Sprintf("/users/%d", s.gone.ID), "")
			if w.Code != http.StatusConflict {
				t.Fatalf("delete with one %s row: %d %s, want 409", bt.name, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), refusedMessage) {
				t.Errorf("the refusal does not carry the message: %s", w.Body.String())
			}
			if !isLive(t, s.db, s.gone.ID) {
				t.Error("the user was soft-deleted although the delete was refused")
			}
			if after := contentCounts(t, s.db, s.gone); !reflect.DeepEqual(after, before) {
				t.Errorf("a refused delete removed content: before %v, after %v", before, after)
			}
		})
	}
}

func TestDeleteUser_AnotherUsersInvoicingRecordsDoNotBlock(t *testing.T) {
	s := newGuardScene(t)
	// The kept user has a row in every blocking table; the deleted user has none.
	for _, bt := range blockingTables {
		mustCreate(t, s.db, bt.row(s.kept.ID))
	}
	if w := fvDo(t, s.router, "DELETE", fmt.Sprintf("/users/%d", s.gone.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("delete of a user with no invoicing rows: %d %s", w.Code, w.Body.String())
	}
	if isLive(t, s.db, s.gone.ID) {
		t.Error("the user was not deleted")
	}
	// Deleted as #11384 does: their content is gone.
	if after := contentCounts(t, s.db, s.gone); after["scratchpads"] != 0 || after["constellation_room_members"] != 0 || after["rooms_created"] != 0 {
		t.Errorf("the deleted user's content is still there: %v", after)
	}
	// And the other user's book is untouched.
	for _, bt := range blockingTables {
		row := bt.row(s.kept.ID)
		if n := count(t, s.db, row, "owner_user_id = ?", s.kept.ID); n != 1 {
			t.Errorf("%s: the kept user has %d row(s), want 1", bt.name, n)
		}
	}
	// The kept user, who does have a book, is the one who is refused.
	if w := fvDo(t, s.router, "DELETE", fmt.Sprintf("/users/%d", s.kept.ID), ""); w.Code != http.StatusConflict {
		t.Errorf("delete of the user with a book: %d, want 409", w.Code)
	}
}

// Categories, tombstones and audit rows are not a book. A user whose only
// invoicing rows are of one of those kinds is deleted like anyone else: the
// categories and tombstones go with the account, and the audit rows stay
// exactly as they were (security's ruling c71589 on system_3 #11471).
func TestDeleteUser_CategoriesTombstonesAndAuditRowsDoNotRefuse(t *testing.T) {
	type seed struct {
		name string
		rows func(s guardScene) []interface{}
	}
	seeds := []seed{}
	for _, rt := range remnantTables {
		rt := rt
		seeds = append(seeds, seed{"only " + rt.name, func(s guardScene) []interface{} { return []interface{}{rt.row(s.gone.ID)} }})
	}
	seeds = append(seeds,
		seed{"only an audit row of their own", func(s guardScene) []interface{} {
			return []interface{}{auditRow(s.gone.ID, invoicingActionExport, invoicingEntityExport, `{"invented":true}`)}
		}},
		// The one audit write whose actor is not the book's owner: an admin
		// fetching another user's failed import.
		seed{"only an admin's download audit row", func(s guardScene) []interface{} {
			return []interface{}{auditRow(s.gone.ID, invoicingActionDownload, invoicingEntityImportFailure, fmt.Sprintf(`{"owner_user_id":%d,"filename":"invented.pdf"}`, s.kept.ID))}
		}},
		seed{"all four kinds at once", func(s guardScene) []interface{} {
			rows := []interface{}{auditRow(s.gone.ID, invoicingActionExport, invoicingEntityExport, `{"invented":true}`)}
			for _, rt := range remnantTables {
				rows = append(rows, rt.row(s.gone.ID))
			}
			return rows
		}},
	)
	for _, tc := range seeds {
		t.Run(tc.name, func(t *testing.T) {
			s := newGuardScene(t)
			for _, row := range tc.rows(s) {
				mustCreate(t, s.db, row)
			}
			// The other user has the same kinds of rows; they must survive.
			for _, rt := range remnantTables {
				mustCreate(t, s.db, rt.row(s.kept.ID))
			}
			var auditBefore []data_models.InvoicingAuditEvent
			if err := s.db.Order("id").Find(&auditBefore).Error; err != nil {
				t.Fatalf("read audit rows: %v", err)
			}

			if w := fvDo(t, s.router, "DELETE", fmt.Sprintf("/users/%d", s.gone.ID), ""); w.Code != http.StatusOK {
				t.Fatalf("delete: %d %s, want 200", w.Code, w.Body.String())
			}
			if isLive(t, s.db, s.gone.ID) {
				t.Error("the user was not deleted")
			}
			for _, rt := range remnantTables {
				if n := count(t, s.db, rt.row(0), "owner_user_id = ?", s.gone.ID); n != 0 {
					t.Errorf("%s: %d row(s) of the deleted user are left, want 0", rt.name, n)
				}
				if n := count(t, s.db, rt.row(0), "owner_user_id = ?", s.kept.ID); n != 1 {
					t.Errorf("%s: the other user has %d row(s), want 1", rt.name, n)
				}
			}
			var auditAfter []data_models.InvoicingAuditEvent
			if err := s.db.Order("id").Find(&auditAfter).Error; err != nil {
				t.Fatalf("read audit rows: %v", err)
			}
			if !reflect.DeepEqual(auditAfter, auditBefore) {
				t.Errorf("the audit rows changed: before %+v, after %+v", auditBefore, auditAfter)
			}
		})
	}
}

// A refused delete removes nothing, the remnants included: the categories and
// tombstones of a user who does have a book stay until the book is dealt with.
func TestDeleteUser_ARefusedDeleteKeepsCategoriesAndTombstones(t *testing.T) {
	s := newGuardScene(t)
	mustCreate(t, s.db, blockingTables[2].row(s.gone.ID)) // one client: a book
	for _, rt := range remnantTables {
		mustCreate(t, s.db, rt.row(s.gone.ID))
	}
	if w := fvDo(t, s.router, "DELETE", fmt.Sprintf("/users/%d", s.gone.ID), ""); w.Code != http.StatusConflict {
		t.Fatalf("delete: %d, want 409", w.Code)
	}
	for _, rt := range remnantTables {
		if n := count(t, s.db, rt.row(0), "owner_user_id = ?", s.gone.ID); n != 1 {
			t.Errorf("%s: %d row(s) after a refused delete, want 1", rt.name, n)
		}
	}
}
