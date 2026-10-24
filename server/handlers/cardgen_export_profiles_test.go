package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newExportProfileRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&data_models.CardExportProfile{})
	data_models.SeedCardExportProfiles(db)
	t.Cleanup(func() { db.Close() })
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("role", "Admin")
		c.Set("user_id", uint(7))
		c.Next()
	})
	g := r.Group("/cardgen")
	g.GET("/export-profiles", ListCardExportProfilesHandler)
	g.POST("/export-profiles", CreateCardExportProfileHandler)
	g.GET("/export-profiles/:id", GetCardExportProfileHandler)
	g.PUT("/export-profiles/:id", UpdateCardExportProfileHandler)
	g.DELETE("/export-profiles/:id", DeleteCardExportProfileHandler)
	return r
}

func exportDo(t *testing.T, r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestCardExportProfiles_CRUD(t *testing.T) {
	r := newExportProfileRouter(t)
	w, out := exportDo(t, r, http.MethodGet, "/cardgen/export-profiles", "")
	if w.Code != http.StatusOK || len(out["profiles"].([]any)) != 3 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	first := out["profiles"].([]any)[0].(map[string]any)
	if first["spec"].(map[string]any)["sheet"].(map[string]any)["page_w_mm"] != 210.0 {
		t.Errorf("preset sheet not expanded: %v", first["spec"])
	}

	w, out = exportDo(t, r, http.MethodPost, "/cardgen/export-profiles", `{"name": "Poker 63x88", "spec": {"card_w_mm": 63, "card_h_mm": 88, "bleed_mm": 3, "sheet": {"page": "A4", "cols": 3, "rows": 3}}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := int(out["profile"].(map[string]any)["id"].(float64))
	if w, _ := exportDo(t, r, http.MethodPost, "/cardgen/export-profiles", `{"name": "Poker 63x88", "spec": {"card_w_mm": 63, "card_h_mm": 88}}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate name: %d", w.Code)
	}
	w, out = exportDo(t, r, http.MethodPost, "/cardgen/export-profiles", `{"name": "Bad", "spec": {"card_w_mm": 63, "card_h_mm": 88, "dpi": 10}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(out["error"].(map[string]any)["message"].(string), "dpi") {
		t.Errorf("bad spec: %d %s", w.Code, w.Body.String())
	}
	w, out = exportDo(t, r, http.MethodPut, "/cardgen/export-profiles/"+itoaExport(id), `{"name": "Poker 63x88 (sheet)", "spec": {"card_w_mm": 63, "card_h_mm": 88, "crop_marks": true}}`)
	if w.Code != http.StatusOK || out["profile"].(map[string]any)["name"] != "Poker 63x88 (sheet)" || out["profile"].(map[string]any)["spec"].(map[string]any)["crop_marks"] != true {
		t.Errorf("update: %d %s", w.Code, w.Body.String())
	}
	if w, _ := exportDo(t, r, http.MethodPut, "/cardgen/export-profiles/"+itoaExport(id), `{"name": "Pinguin 75x105 +3 mm bleed"}`); w.Code != http.StatusConflict {
		t.Errorf("rename onto a preset: %d", w.Code)
	}
	if w, _ := exportDo(t, r, http.MethodDelete, "/cardgen/export-profiles/"+itoaExport(id), ""); w.Code != http.StatusOK {
		t.Errorf("delete: %d", w.Code)
	}
	if w, _ := exportDo(t, r, http.MethodGet, "/cardgen/export-profiles/"+itoaExport(id), ""); w.Code != http.StatusNotFound {
		t.Errorf("get deleted: %d", w.Code)
	}
	if w, _ := exportDo(t, r, http.MethodGet, "/cardgen/export-profiles/abc", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d", w.Code)
	}
}

func itoaExport(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}
