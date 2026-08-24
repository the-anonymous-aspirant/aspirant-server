package handlers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// setupProfileTestDB seeds an in-memory sqlite with a User role and two users
// (bob, carol). UserDisplayName is migrated before the users are created so the
// User.AfterCreate hook opens each user's initial display-name row — the real
// production path, so display-name reads/writes exercise the temporal table.
func setupProfileTestDB(t *testing.T) (*gorm.DB, uint, uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.AutoMigrate(&data_models.Role{}, &data_models.UserDisplayName{}, &data_models.User{})

	userRole := data_models.Role{RoleName: "User", RoleDescription: "Standard"}
	if err := db.Create(&userRole).Error; err != nil {
		t.Fatalf("seed role: %v", err)
	}
	bob := data_models.User{Username: "bob", Email: "bob@example.com", RoleID: userRole.ID}
	carol := data_models.User{Username: "carol", Email: "carol@example.com", RoleID: userRole.ID}
	if err := db.Create(&bob).Error; err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	if err := db.Create(&carol).Error; err != nil {
		t.Fatalf("seed carol: %v", err)
	}
	return db, bob.ID, carol.ID
}

// profileEngine builds a single engine with all profile routes, injecting db +
// storage and (when setAuth) a JWT-style role/user_id context.
func profileEngine(db *gorm.DB, store *storage.LocalStorage, setAuth bool, role string, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		if store != nil {
			c.Set("storage", store)
		}
		if setAuth {
			c.Set("role", role)
			c.Set("user_id", userID)
		}
		c.Next()
	})
	r.GET("/profile", GetMeHandler)
	r.PATCH("/profile", PatchMeHandler)
	r.PUT("/profile/avatar", PutMeAvatarHandler)
	r.DELETE("/profile/avatar", DeleteMeAvatarHandler)
	r.GET("/data_models/users/:id/avatar", GetUserAvatarHandler)
	return r
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func multipartImage(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

// decodeData pulls the `data` object out of the standard success envelope.
func decodeData(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v — body=%s", err, body)
	}
	return env.Data
}

func TestGetMe_ReturnsOwnProfile(t *testing.T) {
	db, bobID, _ := setupProfileTestDB(t)
	defer db.Close()

	r := profileEngine(db, nil, true, "User", bobID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/profile", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w.Body.Bytes())
	if data["username"] != "bob" {
		t.Errorf("username: want bob, got %v", data["username"])
	}
	if data["display_name"] != "bob" {
		t.Errorf("display_name: want bob (from AfterCreate), got %v", data["display_name"])
	}
	if data["avatar_url"] != "" {
		t.Errorf("avatar_url: want empty for no avatar, got %v", data["avatar_url"])
	}
	if _, ok := data["CreatedAt"]; !ok {
		t.Errorf("member-since (CreatedAt) missing from profile: %s", w.Body.String())
	}
}

func TestGetMe_Unauthenticated401(t *testing.T) {
	db, _, _ := setupProfileTestDB(t)
	defer db.Close()

	r := profileEngine(db, nil, false, "", 0) // no user_id set
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/profile", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d — %s", w.Code, w.Body.String())
	}
}

func TestPatchMe_UpdatesDisplayNameAndIsScopedToCaller(t *testing.T) {
	db, bobID, carolID := setupProfileTestDB(t)
	defer db.Close()

	r := profileEngine(db, nil, true, "User", bobID)
	body := strings.NewReader(`{"display_name":"Bobby"}`)
	req := httptest.NewRequest(http.MethodPatch, "/profile", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", w.Code, w.Body.String())
	}
	// The temporal display-name row moved for bob...
	if got := data_models.CurrentDisplayName(db, bobID); got != "Bobby" {
		t.Errorf("bob display name: want Bobby, got %q", got)
	}
	// ...and only bob's — carol is untouched (scoped to session user_id, never a body id).
	if got := data_models.CurrentDisplayName(db, carolID); got != "carol" {
		t.Errorf("carol display name must be unchanged, got %q", got)
	}
}

func TestPatchMe_EmptyDisplayNameRejected(t *testing.T) {
	db, bobID, _ := setupProfileTestDB(t)
	defer db.Close()

	r := profileEngine(db, nil, true, "User", bobID)
	req := httptest.NewRequest(http.MethodPatch, "/profile", strings.NewReader(`{"display_name":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for blank display name, got %d — %s", w.Code, w.Body.String())
	}
}

func TestAvatar_UploadServeAndClearRoundTrip(t *testing.T) {
	db, bobID, _ := setupProfileTestDB(t)
	defer db.Close()
	store, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	r := profileEngine(db, store, true, "User", bobID)

	// Upload a valid PNG.
	png := pngBytes(t)
	buf, ctype := multipartImage(t, "image", "me.png", png)
	req := httptest.NewRequest(http.MethodPut, "/profile/avatar", buf)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: want 200, got %d — %s", w.Code, w.Body.String())
	}
	avatarURL, _ := decodeData(t, w.Body.Bytes())["avatar_url"].(string)
	if avatarURL == "" || !strings.Contains(avatarURL, "/avatar?v=") {
		t.Fatalf("upload: expected versioned avatar_url, got %q", avatarURL)
	}

	// The user row now carries an etag, and the public DTO exposes the URL —
	// this is what the message-board consumer reads.
	var bob data_models.User
	db.First(&bob, bobID)
	if bob.AvatarETag == "" {
		t.Fatalf("avatar_etag not persisted on user row")
	}
	if pub := bob.ToPublicResponse(); pub.AvatarURL == "" {
		t.Errorf("PublicUserResponse.avatar_url should be set after upload (message-board propagation)")
	}
	// #4223 item 2: the ADMIN DTO must carry it too — GetAllUsersHandler returns
	// UserResponse (not the public one) to an admin, so an admin viewing the
	// message board would otherwise see the placeholder for every author,
	// including their own drawn icon.
	if adminDTO := bob.ToResponse(); adminDTO.AvatarURL == "" {
		t.Errorf("UserResponse.avatar_url should be set after upload (admin message-board propagation, #4223)")
	}

	// Serve the avatar to any authenticated caller.
	sw := httptest.NewRecorder()
	r.ServeHTTP(sw, httptest.NewRequest(http.MethodGet, "/data_models/users/"+strconv.Itoa(int(bobID))+"/avatar", nil))
	if sw.Code != http.StatusOK {
		t.Fatalf("serve avatar: want 200, got %d — %s", sw.Code, sw.Body.String())
	}
	if ct := sw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("serve avatar: want image content-type, got %q", ct)
	}
	if !bytes.Equal(sw.Body.Bytes(), png) {
		t.Errorf("serve avatar: bytes not identical to upload")
	}

	// Clear it — the pointer is cleared and the serve route 404s.
	cw := httptest.NewRecorder()
	r.ServeHTTP(cw, httptest.NewRequest(http.MethodDelete, "/profile/avatar", nil))
	if cw.Code != http.StatusOK {
		t.Fatalf("clear avatar: want 200, got %d — %s", cw.Code, cw.Body.String())
	}
	db.First(&bob, bobID)
	if bob.AvatarETag != "" {
		t.Errorf("avatar_etag should be cleared, got %q", bob.AvatarETag)
	}
	gw := httptest.NewRecorder()
	r.ServeHTTP(gw, httptest.NewRequest(http.MethodGet, "/data_models/users/"+strconv.Itoa(int(bobID))+"/avatar", nil))
	if gw.Code != http.StatusNotFound {
		t.Errorf("serve avatar after clear: want 404, got %d", gw.Code)
	}
}

func TestAvatar_RejectsNonImage(t *testing.T) {
	db, bobID, _ := setupProfileTestDB(t)
	defer db.Close()
	store, _ := storage.NewLocalStorage(t.TempDir())
	r := profileEngine(db, store, true, "User", bobID)

	buf, ctype := multipartImage(t, "image", "notes.txt", []byte("this is plainly not an image file at all"))
	req := httptest.NewRequest(http.MethodPut, "/profile/avatar", buf)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-image, got %d — %s", w.Code, w.Body.String())
	}
	var bob data_models.User
	db.First(&bob, bobID)
	if bob.AvatarETag != "" {
		t.Errorf("rejected upload must not set avatar_etag, got %q", bob.AvatarETag)
	}
}

func TestGetUserAvatar_NoAvatar404(t *testing.T) {
	db, bobID, _ := setupProfileTestDB(t)
	defer db.Close()
	store, _ := storage.NewLocalStorage(t.TempDir())
	r := profileEngine(db, store, true, "User", bobID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/data_models/users/"+strconv.Itoa(int(bobID))+"/avatar", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for user without avatar, got %d", w.Code)
	}
}
