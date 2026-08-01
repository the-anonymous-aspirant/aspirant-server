package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newGameTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&data_models.Role{}, &data_models.User{}, &data_models.UserDisplayName{}, &data_models.GameScore{})
	return db
}

// The public /games/scores leaderboard is unauthenticated (routes.go), so it
// must publish the display name, never the login username — otherwise it hands
// anonymous callers a valid username list (security-finding #3094).
func TestGameScoresEmitsDisplayNameNotLoginUsername(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newGameTestDB(t)
	defer db.Close()

	role := data_models.Role{RoleName: "Admin", RoleDescription: "Administrator"}
	if err := db.Create(&role).Error; err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}

	// The AfterCreate hook opens a display row = "login_secret"; then we switch
	// the public display name so it deliberately differs from the credential.
	user := data_models.User{Username: "login_secret", Email: "hero@example.com", RoleID: role.ID}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
	if err := data_models.SetDisplayName(db, user.ID, "PublicHero"); err != nil {
		t.Fatalf("failed to set display name: %v", err)
	}

	score := data_models.GameScore{UserID: int(user.ID), Game: "word_weaver", Score: 42}
	if err := db.Create(&score).Error; err != nil {
		t.Fatalf("failed to seed score: %v", err)
	}

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("db", db); c.Next() })
	router.GET("/games/scores", GetGameScoresHandler)

	req, _ := http.NewRequest(http.MethodGet, "/games/scores?game=word_weaver", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v (body=%s)", err, w.Body.String())
	}
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 leaderboard row, got %d (body=%s)", len(resp.Items), w.Body.String())
	}

	got := resp.Items[0]["username"]
	if got != "PublicHero" {
		t.Errorf("leaderboard username = %v, want the display name %q", got, "PublicHero")
	}
	if got == "login_secret" {
		t.Errorf("leaderboard leaked the login username %q", "login_secret")
	}
}
