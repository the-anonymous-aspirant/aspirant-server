package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// Remembered form values (system_3 #11190, §3.154). Every value here is
// invented (Decision 8); nothing runs against a real account.

const kvittoPath = "/users/me/form-values/kvitto"

func setupFormValuesDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.UserFormValues{}, &data_models.Role{}, &data_models.User{})
	t.Cleanup(func() { db.Close() })
	return db
}

// formValuesRouter fakes AuthMiddleware: the session's user and role are set
// in the context, the way every authRoutes handler sees them.
func formValuesRouter(db *gorm.DB, userID uint, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Set("role", role)
		c.Next()
	})
	r.GET("/users/me/form-values/:tool", GetFormValuesHandler)
	r.PUT("/users/me/form-values/:tool", PutFormValuesHandler)
	r.DELETE("/users/me/form-values/:tool", DeleteFormValuesHandler)
	return r
}

func fvDo(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req, _ = http.NewRequest(method, path, nil)
	} else {
		req, _ = http.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

func fvRows(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int
	db.Model(&data_models.UserFormValues{}).Count(&n)
	return n
}

// An invented, legal kvitto body.
const validKvitto = `{"address":"Testgatan 1, 123 45 Exempelstad","monthly_amount":4321.5,"recipient":"Ada Exempel","payer":"Bo Prov"}`

func TestFormValues_UnknownToolIs404AndStoresNothing(t *testing.T) {
	db := setupFormValuesDB(t)
	r := formValuesRouter(db, 1, "Admin")
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		w := fvDo(t, r, method, "/users/me/form-values/not-a-tool", validKvitto)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s unknown tool: want 404, got %d", method, w.Code)
		}
	}
	if n := fvRows(t, db); n != 0 {
		t.Fatalf("an unknown tool stored %d rows", n)
	}
}

func TestFormValues_BelowTierIsRefused(t *testing.T) {
	db := setupFormValuesDB(t)
	for _, role := range []string{"Member", "Viewer", "Blocked", ""} {
		r := formValuesRouter(db, 7, role)
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			w := fvDo(t, r, method, kvittoPath, validKvitto)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s as %q: want 403, got %d", method, role, w.Code)
			}
		}
	}
	if n := fvRows(t, db); n != 0 {
		t.Fatalf("a below-tier user stored %d rows", n)
	}
}

// The user comes from the session. Nothing in the path, query or body can name
// another one, even for an admin.
func TestFormValues_NoUserCanBeNamedInTheRequest(t *testing.T) {
	db := setupFormValuesDB(t)
	alice := formValuesRouter(db, 1, "Admin")
	bob := formValuesRouter(db, 2, "Admin")
	if w := fvDo(t, alice, "PUT", kvittoPath, validKvitto); w.Code != http.StatusOK {
		t.Fatalf("alice's write: %d %s", w.Code, w.Body.String())
	}

	// A query parameter naming alice changes nothing: bob reads his own (empty).
	w := fvDo(t, bob, "GET", kvittoPath+"?user_id=1&user=1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"values":null`) {
		t.Fatalf("bob with ?user_id=1: want his own empty record, got %d %s", w.Code, w.Body.String())
	}
	// A body field naming a user is an extra key: refused, nothing stored.
	w = fvDo(t, bob, "PUT", kvittoPath, `{"user_id":1,"address":"x","monthly_amount":1,"recipient":"x","payer":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a body naming a user: want 400, got %d", w.Code)
	}
	// A path that tries to name a user does not route at all.
	if w := fvDo(t, bob, "GET", "/users/1/form-values/kvitto", ""); w.Code != http.StatusNotFound {
		t.Fatalf("/users/1/...: want 404 (no such route), got %d", w.Code)
	}
	// Bob deleting "his" record leaves alice's alone.
	fvDo(t, bob, "DELETE", kvittoPath+"?user_id=1", "")
	if fv, _ := data_models.GetFormValues(db, 1, "kvitto"); fv == nil {
		t.Fatal("bob's DELETE removed alice's record")
	}
	if n := fvRows(t, db); n != 1 {
		t.Fatalf("want exactly alice's one row, got %d", n)
	}
}

func TestFormValues_ExtraKeyIsRefused(t *testing.T) {
	db := setupFormValuesDB(t)
	r := formValuesRouter(db, 1, "Admin")
	w := fvDo(t, r, "PUT", kvittoPath, `{"address":"a","monthly_amount":1,"recipient":"r","payer":"p","year":2026}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an extra key: want 400, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "year") {
		t.Errorf("the refusal echoed the key it was sent: %s", w.Body.String())
	}
	if n := fvRows(t, db); n != 0 {
		t.Fatalf("an extra key stored %d rows", n)
	}
}

func TestFormValues_OverLengthTextNamesTheFieldNotTheValue(t *testing.T) {
	db := setupFormValuesDB(t)
	r := formValuesRouter(db, 1, "Admin")
	cases := map[string]string{
		"address":   strings.Repeat("Q", kvittoAddressMax+1),
		"recipient": strings.Repeat("Q", kvittoNameMax+1),
		"payer":     strings.Repeat("Q", kvittoNameMax+1),
	}
	for field, long := range cases {
		body := map[string]interface{}{"address": "a", "monthly_amount": 1, "recipient": "r", "payer": "p"}
		body[field] = long
		raw, _ := json.Marshal(body)
		w := fvDo(t, r, "PUT", kvittoPath, string(raw))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s over its cap: want 400, got %d", field, w.Code)
		}
		if !strings.Contains(w.Body.String(), field) {
			t.Errorf("%s: the refusal does not name the field: %s", field, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "QQQQ") {
			t.Errorf("%s: the refusal echoed the value", field)
		}
	}
	// Exactly at the cap is accepted, so the bound is the cap and not one below.
	ok := fmt.Sprintf(`{"address":%q,"monthly_amount":1,"recipient":"r","payer":"p"}`, strings.Repeat("Q", kvittoAddressMax))
	if w := fvDo(t, r, "PUT", kvittoPath, ok); w.Code != http.StatusOK {
		t.Fatalf("an address of exactly %d characters: want 200, got %d", kvittoAddressMax, w.Code)
	}
}

func TestFormValues_TypesAndAmountBounds(t *testing.T) {
	db := setupFormValuesDB(t)
	r := formValuesRouter(db, 1, "Admin")
	bad := map[string]string{
		"monthly_amount": `{"address":"a","monthly_amount":"1000","recipient":"r","payer":"p"}`,
		"payer":          `{"address":"a","monthly_amount":1,"recipient":"r","payer":42}`,
	}
	for field, body := range bad {
		w := fvDo(t, r, "PUT", kvittoPath, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), field) {
			t.Errorf("wrong type for %s: want 400 naming it, got %d %s", field, w.Code, w.Body.String())
		}
	}
	for _, amount := range []string{"0", "-5", "1000000.01", "12.345"} {
		w := fvDo(t, r, "PUT", kvittoPath, `{"address":"a","monthly_amount":`+amount+`,"recipient":"r","payer":"p"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("monthly_amount %s: want 400, got %d", amount, w.Code)
		}
	}
	if w := fvDo(t, r, "PUT", kvittoPath, `{"address":"a","monthly_amount":1000000,"recipient":"r","payer":"p"}`); w.Code != http.StatusOK {
		t.Errorf("monthly_amount at the cap: want 200, got %d", w.Code)
	}
	if w := fvDo(t, r, "PUT", kvittoPath, validKvitto+`{}`); w.Code != http.StatusBadRequest {
		t.Errorf("trailing data after the object: want 400, got %d", w.Code)
	}
}

func TestFormValues_RoundTripHoldsExactlyTheFourKeys(t *testing.T) {
	db := setupFormValuesDB(t)
	r := formValuesRouter(db, 1, "Admin")
	if w := fvDo(t, r, "GET", kvittoPath, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"values":null`) {
		t.Fatalf("nothing stored: want 200 with null values, got %d %s", w.Code, w.Body.String())
	}
	if w := fvDo(t, r, "PUT", kvittoPath, validKvitto); w.Code != http.StatusOK {
		t.Fatalf("write: %d %s", w.Code, w.Body.String())
	}
	w := fvDo(t, r, "GET", kvittoPath, "")
	var resp struct {
		Values map[string]interface{} `json:"values"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(resp.Values) != 4 {
		t.Fatalf("want exactly 4 keys, got %v", resp.Values)
	}
	if resp.Values["recipient"] != "Ada Exempel" || resp.Values["monthly_amount"] != 4321.5 {
		t.Fatalf("round trip changed the values: %v", resp.Values)
	}
}

func TestFormValues_DeleteRemovesTheRow(t *testing.T) {
	db := setupFormValuesDB(t)
	r := formValuesRouter(db, 1, "Admin")
	fvDo(t, r, "PUT", kvittoPath, validKvitto)
	if n := fvRows(t, db); n != 1 {
		t.Fatalf("before DELETE: want 1 row, got %d", n)
	}
	if w := fvDo(t, r, "DELETE", kvittoPath, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: want 204, got %d", w.Code)
	}
	// Removed, not emptied: the row is gone, and no unscoped copy survives.
	var n int
	db.Unscoped().Model(&data_models.UserFormValues{}).Count(&n)
	if n != 0 {
		t.Fatalf("after DELETE: want 0 rows, got %d", n)
	}
	if w := fvDo(t, r, "DELETE", kvittoPath, ""); w.Code != http.StatusNoContent {
		t.Fatalf("a second DELETE: want 204, got %d", w.Code)
	}
}

// Deleting a user is a soft delete (gorm.Model), so the record must go through
// the real delete path, not a cascade that a soft delete never fires.
func TestFormValues_DeletingTheUserRemovesTheirRecord(t *testing.T) {
	db := setupFormValuesDB(t)
	role := data_models.Role{RoleName: "Admin"}
	db.Create(&role)
	target := data_models.User{Username: "invented-user", Email: "invented@example.test", RoleID: role.ID}
	if err := db.Create(&target).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	other := data_models.User{Username: "invented-other", Email: "other@example.test", RoleID: role.ID}
	db.Create(&other)
	if _, err := data_models.UpsertFormValues(db, target.ID, "kvitto", `{"address":"a"}`); err != nil {
		t.Fatalf("seed target's record: %v", err)
	}
	if _, err := data_models.UpsertFormValues(db, other.ID, "kvitto", `{"address":"b"}`); err != nil {
		t.Fatalf("seed other's record: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", uint(999)) // an admin who is neither user
		c.Next()
	})
	r.DELETE("/users/:id", DeleteUserHandler)
	w := fvDo(t, r, "DELETE", fmt.Sprintf("/users/%d", target.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", w.Code, w.Body.String())
	}

	if fv, _ := data_models.GetFormValues(db, target.ID, "kvitto"); fv != nil {
		t.Fatal("the deleted user's record survived")
	}
	if fv, _ := data_models.GetFormValues(db, other.ID, "kvitto"); fv == nil {
		t.Fatal("deleting one user removed another user's record")
	}
	// And the user itself is soft-deleted, as before.
	var u data_models.User
	db.Unscoped().First(&u, target.ID)
	if u.DeletedAt == nil {
		t.Fatal("the user row was not soft-deleted")
	}
}

func TestFormValues_NoStoreOnEveryResponse(t *testing.T) {
	db := setupFormValuesDB(t)
	admin := formValuesRouter(db, 1, "Admin")
	member := formValuesRouter(db, 2, "Member")
	cases := []struct {
		name   string
		r      *gin.Engine
		method string
		path   string
		body   string
		code   int
	}{
		{"read", admin, "GET", kvittoPath, "", 200},
		{"write", admin, "PUT", kvittoPath, validKvitto, 200},
		{"refused write", admin, "PUT", kvittoPath, `{"nope":1}`, 400},
		{"delete", admin, "DELETE", kvittoPath, "", 204},
		{"below tier", member, "GET", kvittoPath, "", 403},
		{"unknown tool", admin, "GET", "/users/me/form-values/nope", "", 404},
	}
	for _, tc := range cases {
		w := fvDo(t, tc.r, tc.method, tc.path, tc.body)
		if w.Code != tc.code {
			t.Errorf("%s: want %d, got %d", tc.name, tc.code, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control is %q, want no-store", tc.name, got)
		}
	}
}
