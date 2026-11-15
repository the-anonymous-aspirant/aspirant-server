package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"aspirant-online/server/handlers"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// TestFormValuesNeverReachTheLogs drives one accepted write and one refused
// write of remembered form values (system_3 #11190, §3.154: "values appear in
// no request or error log") through the real SetupMiddleware access log, with
// the std log captured too, and asserts that no value appears in either.
// Every value is an invented marker (Decision 8).
func TestFormValuesNeverReachTheLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var access, std strings.Builder
	prevWriter := gin.DefaultWriter
	gin.DefaultWriter = &access
	t.Cleanup(func() { gin.DefaultWriter = prevWriter })
	prevLog := log.Writer()
	log.SetOutput(&std)
	t.Cleanup(func() { log.SetOutput(prevLog) })

	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.AutoMigrate(&data_models.UserFormValues{})

	r := gin.New()
	SetupMiddleware(r)
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", uint(1))
		c.Set("role", "Admin")
		c.Next()
	})
	r.PUT("/users/me/form-values/:tool", handlers.PutFormValuesHandler)

	const accepted = "MARKER-ACCEPTED-7f3a"
	const refused = "MARKER-REFUSED-9c1e"
	send := func(body string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/users/me/form-values/kvitto", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := send(`{"address":"` + accepted + `","monthly_amount":1,"recipient":"` + accepted + `","payer":"` + accepted + `"}`); code != http.StatusOK {
		t.Fatalf("the accepted write: want 200, got %d", code)
	}
	// Over the address cap, so it is refused with a field-naming error.
	if code := send(`{"address":"` + refused + strings.Repeat("Z", 400) + `","monthly_amount":1,"recipient":"` + refused + `","payer":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("the refused write: want 400, got %d", code)
	}

	// Positive control: both requests were logged, so the absence checks
	// below cannot pass on an empty log.
	if n := strings.Count(access.String(), `"/users/me/form-values/kvitto"`); n != 2 {
		t.Fatalf("want 2 access-log lines for the route, got %d: %q", n, access.String())
	}
	if !strings.Contains(std.String(), "Error response: 400") {
		t.Fatalf("the refusal's error line was not captured: %q", std.String())
	}

	for _, marker := range []string{accepted, refused, "ZZZZ"} {
		if strings.Contains(access.String(), marker) {
			t.Errorf("a value reached the access log: %q", marker)
		}
		if strings.Contains(std.String(), marker) {
			t.Errorf("a value reached the std log: %q", marker)
		}
	}
}
