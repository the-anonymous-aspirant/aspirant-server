package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// --- callerIsAdmin helper tests ---

func TestCallerIsAdmin_TrueWhenRoleAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("role", "Admin")
	if !callerIsAdmin(c) {
		t.Fatalf("expected true for Admin role")
	}
}

func TestCallerIsAdmin_FalseForOtherRoles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, role := range []string{"Trusted", "User", "", "admin"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("role", role)
		if callerIsAdmin(c) {
			t.Fatalf("expected false for role %q", role)
		}
	}
}

func TestCallerIsAdmin_FalseWhenRoleAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if callerIsAdmin(c) {
		t.Fatalf("expected false when role absent")
	}
}

// setupUserTestDB seeds an in-memory sqlite with an Admin and a User
// role plus two users (alice = Admin, bob = User) and returns the db.
func setupUserTestDB(t *testing.T) (*gorm.DB, uint, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.Role{}, &data_models.User{})

	adminRole := data_models.Role{RoleName: "Admin", RoleDescription: "Administrator"}
	userRole := data_models.Role{RoleName: "User", RoleDescription: "Standard"}
	if err := db.Create(&adminRole).Error; err != nil {
		t.Fatalf("failed to seed admin role: %v", err)
	}
	if err := db.Create(&userRole).Error; err != nil {
		t.Fatalf("failed to seed user role: %v", err)
	}

	alice := data_models.User{Username: "alice", Email: "alice@example.com", RoleID: adminRole.ID, Comment: "admin note"}
	bob := data_models.User{Username: "bob", Email: "bob@example.com", RoleID: userRole.ID, Comment: "user note"}
	if err := db.Create(&alice).Error; err != nil {
		t.Fatalf("failed to seed alice: %v", err)
	}
	if err := db.Create(&bob).Error; err != nil {
		t.Fatalf("failed to seed bob: %v", err)
	}
	return db, alice.ID, bob.ID
}

// routerAs builds a single-route engine whose middleware injects the db
// plus a JWT-style role/user_id context, so a handler runs as that caller.
func routerAs(db *gorm.DB, role string, userID uint, method, path string, h gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("role", role)
		c.Set("user_id", userID)
		c.Next()
	})
	r.Handle(method, path, h)
	return r
}

// --- GetUserHandler item-route DTO behaviour (#3093) ---

// TestGetUserHandler_NonAdminCrossIDReturnsPublic locks the #3093
// consistency fix: a non-Admin looking up another user's id no longer
// gets a 403 (protection that wasn't there — the collection listed the
// same fields), it gets the PII-stripped PublicUserResponse. The #1380
// boundary still holds: no email/comment, and #3093 adds no access_role.
func TestGetUserHandler_NonAdminCrossIDReturnsPublic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, aliceID, bobID := setupUserTestDB(t)
	defer db.Close()

	// bob (non-Admin) looks up alice (a different, Admin, id).
	r := routerAs(db, "User", bobID, http.MethodGet, "/data_models/users/:id", GetUserHandler)
	req := httptest.NewRequest(http.MethodGet, "/data_models/users/"+itoa(aliceID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "alice") {
		t.Errorf("expected username in public response, got %s", body)
	}
	if strings.Contains(body, "access_role") {
		t.Errorf("#3093: access_role must not leak to a non-Admin, got %s", body)
	}
	if strings.Contains(body, "alice@example.com") || strings.Contains(body, "\"email\"") {
		t.Errorf("#1380: email must not leak to a non-Admin, got %s", body)
	}
}

// TestGetUserHandler_AdminGetsFull confirms the Admin path is untouched:
// email, comment, and access_role are all present for an Admin caller.
func TestGetUserHandler_AdminGetsFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, aliceID, bobID := setupUserTestDB(t)
	defer db.Close()

	r := routerAs(db, "Admin", aliceID, http.MethodGet, "/data_models/users/:id", GetUserHandler)
	req := httptest.NewRequest(http.MethodGet, "/data_models/users/"+itoa(bobID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"bob", "bob@example.com", "access_role"} {
		if !strings.Contains(body, want) {
			t.Errorf("Admin response missing %q, got %s", want, body)
		}
	}
}

// --- GetAllUsersHandler collection-route DTO behaviour (#3093) ---

// TestGetAllUsersHandler_NonAdminOmitsAccessRole is the core #3093
// regression lock: the collection route must not disclose access_role
// (nor email/comment) to a non-Admin authenticated caller, so the Admin
// account is not enumerable in bulk.
func TestGetAllUsersHandler_NonAdminOmitsAccessRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _, bobID := setupUserTestDB(t)
	defer db.Close()

	r := routerAs(db, "User", bobID, http.MethodGet, "/data_models/users", GetAllUsersHandler)
	req := httptest.NewRequest(http.MethodGet, "/data_models/users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "alice") || !strings.Contains(body, "bob") {
		t.Errorf("expected both usernames in listing, got %s", body)
	}
	if strings.Contains(body, "access_role") {
		t.Errorf("#3093: access_role must not appear in the non-Admin listing, got %s", body)
	}
	if strings.Contains(body, "@example.com") {
		t.Errorf("#1380: emails must not appear in the non-Admin listing, got %s", body)
	}
}

// TestGetAllUsersHandler_AdminSeesAccessRole confirms the Admin listing
// still carries the full fields.
func TestGetAllUsersHandler_AdminSeesAccessRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, aliceID, _ := setupUserTestDB(t)
	defer db.Close()

	r := routerAs(db, "Admin", aliceID, http.MethodGet, "/data_models/users", GetAllUsersHandler)
	req := httptest.NewRequest(http.MethodGet, "/data_models/users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"access_role", "@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("Admin listing missing %q, got %s", want, body)
		}
	}
}

// itoa is a tiny uint→string helper kept local to the test file so the
// handler tests need not import strconv at call sites.
func itoa(u uint) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}
