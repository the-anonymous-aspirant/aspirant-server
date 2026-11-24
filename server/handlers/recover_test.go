package handlers

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// system_3 #12255: a panicking handler answers 500 in the usual envelope, and
// the transaction it opened is ended, so the next request does not wait on it.
func TestRecoverPanics_PanicAnswers500AndEndsTheTransaction(t *testing.T) {
	db := setupInvoicingDB(t)
	// One connection: a transaction left open would keep it, and the next
	// request would wait forever.
	db.DB().SetMaxOpenConns(1)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RecoverPanics())
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", uint(42))
		c.Next()
	})
	r.POST("/panics", func(c *gin.Context) {
		tx := db.Begin()
		defer tx.RollbackUnlessCommitted()
		if err := tx.Create(&data_models.InvoicingClient{OwnerUserID: 42, Name: "Invented Panic Client"}).Error; err != nil {
			t.Errorf("write inside the transaction: %v", err)
		}
		panic("an invented handler bug")
	})
	r.PUT("/invoicing/profile", PutInvoicingProfileHandler)

	w := within(t, "the panicking request", func() *httptest.ResponseRecorder { return invoicingDo(r, "POST", "/panics", "") })
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"code":"internal_error"`) {
		t.Fatalf("panicking request: got %d %s, want 500 internal_error", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "invented handler bug") {
		t.Errorf("the panic value reached the response: %s", w.Body.String())
	}
	if w := within(t, "the next write", func() *httptest.ResponseRecorder {
		return invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody)
	}); w.Code != http.StatusOK {
		t.Fatalf("the next write: %d %s", w.Code, w.Body.String())
	}
	var n int
	db.Model(&data_models.InvoicingClient{}).Where("name = ?", "Invented Panic Client").Count(&n)
	if n != 0 {
		t.Errorf("the panicking request's write was kept (%d rows); its transaction should have rolled back", n)
	}
}

// Every transaction the server opens is ended on every way out of its
// function, a panic included: `x := y.Begin()` is followed at once by
// `defer x.RollbackUnlessCommitted()`.
func TestEveryBeginIsFollowedByADeferredRollback(t *testing.T) {
	begin := regexp.MustCompile(`^\s*(\w+)\s*:?=\s*\w+\.Begin\(\)\s*$`)
	anyBegin := regexp.MustCompile(`\.Begin\(\)`)
	found := 0
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		var lines []string
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		for i, line := range lines {
			if !anyBegin.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			m := begin.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%s:%d: a Begin() not of the form `tx := db.Begin()`: %s", path, i+1, strings.TrimSpace(line))
				continue
			}
			found++
			want := "defer " + m[1] + ".RollbackUnlessCommitted()"
			if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != want {
				t.Errorf("%s:%d: %s is not followed by `%s`", path, i+1, strings.TrimSpace(line), want)
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if found < 30 {
		t.Errorf("found only %d Begin() calls; is the walk reading the server's sources?", found)
	}
}
