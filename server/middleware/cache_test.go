package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func cacheTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	file := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.4 test"), 0o600); err != nil {
		t.Fatalf("writing the test file: %v", err)
	}
	r := gin.New()
	r.Use(NoStoreByDefault())
	r.GET("/export/book.csv", func(c *gin.Context) {
		c.Header("Content-Disposition", `attachment; filename="book.csv"`)
		c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte("a;b\n"))
	})
	// The files manager serves from disk with c.File (http.ServeFile).
	r.GET("/files/download/:filename", func(c *gin.Context) { c.File(file) })
	r.GET("/own-value.png", func(c *gin.Context) {
		c.Header("Cache-Control", "private, max-age=86400")
		c.Data(http.StatusOK, "image/png", []byte("png"))
	})
	r.GET("/refused.csv", func(c *gin.Context) { c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "no"}) })
	return r
}

func cacheControlOf(r *gin.Engine, path string) (int, string) {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Header().Get("Cache-Control")
}

// #11494: each shape of response says no-store, a file download included.
func TestNoStoreByDefault_CoversDownloadsRefusalsAndUnknownPaths(t *testing.T) {
	r := cacheTestRouter(t)
	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"a CSV written with c.Data", "/export/book.csv", http.StatusOK},
		{"a file served from disk with c.File", "/files/download/report.pdf", http.StatusOK},
		{"a refusal", "/refused.csv", http.StatusUnauthorized},
		{"a path no route matches", "/nothing-here.zip", http.StatusNotFound},
	} {
		code, got := cacheControlOf(r, tc.path)
		if code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.status)
		}
		if got != "no-store" {
			t.Errorf("%s: Cache-Control is %q, want no-store", tc.name, got)
		}
	}
}

// A handler that names its own policy keeps it: the default never overrides.
func TestNoStoreByDefault_AHandlersOwnValueWins(t *testing.T) {
	code, got := cacheControlOf(cacheTestRouter(t), "/own-value.png")
	if code != http.StatusOK || got != "private, max-age=86400" {
		t.Errorf("status %d, Cache-Control %q; want 200 and the handler's own value", code, got)
	}
}
