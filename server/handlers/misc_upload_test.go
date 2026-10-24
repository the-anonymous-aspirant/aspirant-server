package handlers

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
)

// system_3 #8657: the upload and delete handlers take their storage key from
// the client. The store now refuses a key that leaves ASSET_BASE_PATH and the
// handlers answer 400 for it; these tests pin that, and that nothing is
// written outside a temp base.

func uploadRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	parent := t.TempDir()
	store, err := storage.NewLocalStorage(filepath.Join(parent, "assets"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("storage", store)
		c.Set("role", "Admin")
		c.Set("user_id", uint(1))
		c.Next()
	})
	r.POST("/assets/upload", UploadImageHandler)
	r.DELETE("/assets", DeleteAssetHandler)
	return r, parent
}

func postUpload(t *testing.T, r *gin.Engine, key string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("path", key)
	part, err := mw.CreateFormFile("image", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/assets/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func filesOutsideAssets(t *testing.T, parent string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, filepath.Join(parent, "assets")+string(filepath.Separator)) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestUploadImageHandler_TraversalKeyIs400AndWritesNothing(t *testing.T) {
	r, parent := uploadRouter(t)
	for _, key := range []string{"../outside.txt", "../../etc/x", "a/../../x", "/abs.txt"} {
		w := postUpload(t, r, key, []byte("payload"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("path %q: want 400, got %d body=%s", key, w.Code, w.Body.String())
		}
	}
	if files := filesOutsideAssets(t, parent); len(files) != 0 {
		t.Fatalf("traversal keys wrote outside the store: %v", files)
	}
	// Positive control: a key inside the store still round-trips.
	if w := postUpload(t, r, "images/a.png", []byte("payload")); w.Code != http.StatusOK {
		t.Fatalf("inside key: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(parent, "assets", "images", "a.png")); err != nil {
		t.Fatalf("inside key not stored: %v", err)
	}
}

func TestDeleteAssetHandler_TraversalKeyIs400(t *testing.T) {
	r, parent := uploadRouter(t)
	// A file next to the store that a traversal delete would reach.
	victim := filepath.Join(parent, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/assets?key=../victim.txt", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("traversal delete removed the file outside the store: %v", err)
	}
}
