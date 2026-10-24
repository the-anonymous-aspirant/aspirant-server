package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// #8643: template and type routes, driven as an Admin with db, role and
// user_id set (the gate is cardgen_templates_gate_test.go's job).

type cardTemplateFixture struct {
	db *gorm.DB
	r  *gin.Engine
}

func newCardTemplateFixture(t *testing.T) *cardTemplateFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&data_models.CardType{}, &data_models.CardTemplate{}, &data_models.CardTemplateVersion{})
	t.Cleanup(func() { db.Close() })
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("role", "Admin")
		c.Set("user_id", uint(7))
		c.Next()
	})
	g := r.Group("/cardgen")
	g.GET("/types", ListCardTypesHandler)
	g.POST("/types", CreateCardTypeHandler)
	g.PUT("/types/:id", UpdateCardTypeHandler)
	g.GET("/templates", ListCardTemplatesHandler)
	g.POST("/templates", CreateCardTemplateHandler)
	g.GET("/templates/:id", GetCardTemplateHandler)
	g.PUT("/templates/:id", UpdateCardTemplateHandler)
	g.DELETE("/templates/:id", DeleteCardTemplateHandler)
	g.GET("/templates/:id/versions", ListCardTemplateVersionsHandler)
	g.POST("/templates/:id/versions", CreateCardTemplateVersionHandler)
	g.POST("/templates/:id/versions/:n/restore", RestoreCardTemplateVersionHandler)
	g.GET("/templates/:id/resolved", ResolveCardTemplateHandler)
	return &cardTemplateFixture{db: db, r: r}
}

func (f *cardTemplateFixture) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *cardTemplateFixture) mustCreate(t *testing.T, slug string, parent *uint, spec string) uint {
	t.Helper()
	body := map[string]any{"slug": slug, "name": slug, "spec": json.RawMessage(spec)}
	if parent != nil {
		body["parent_template_id"] = *parent
	}
	b, _ := json.Marshal(body)
	w, out := f.do(t, http.MethodPost, "/cardgen/templates", string(b))
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", slug, w.Code, w.Body.String())
	}
	return uint(out["template"].(map[string]any)["id"].(float64))
}

func TestCardTemplates_CreateVersionRestoreResolve(t *testing.T) {
	f := newCardTemplateFixture(t)
	base := f.mustCreate(t, "base", nil, `{"size": {"w_mm": 105, "h_mm": 74.25}, "elements": {"title": {"kind": "text", "size_pt": 12, "color": "#111111"}}}`)
	action := f.mustCreate(t, "action", &base, `{"elements": {"title": {"color": "#222222"}}}`)

	w, out := f.do(t, http.MethodGet, "/cardgen/templates/"+itoaCardgen(action), "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	tm := out["template"].(map[string]any)
	if tm["parent_slug"] != "base" || tm["current_version_no"] != 1.0 || tm["spec"] == nil {
		t.Errorf("get = %v", tm)
	}

	w, out = f.do(t, http.MethodPost, "/cardgen/templates/"+itoaCardgen(action)+"/versions", `{"spec": {"elements": {"title": {"color": "#333333"}}}, "note": "darker"}`)
	if w.Code != http.StatusCreated || out["version"].(map[string]any)["version_no"] != 2.0 {
		t.Fatalf("v2: %d %s", w.Code, w.Body.String())
	}
	w, out = f.do(t, http.MethodGet, "/cardgen/templates/"+itoaCardgen(action)+"/resolved", "")
	if w.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", w.Code, w.Body.String())
	}
	res := out["resolved"].(map[string]any)
	els := res["elements"].([]any)
	title := els[0].(map[string]any)
	if title["props"].(map[string]any)["color"] != "#333333" || title["provenance"].(map[string]any)["color"] != "action" || title["provenance"].(map[string]any)["size_pt"] != "base" {
		t.Errorf("resolved title = %v", title)
	}
	if res["size"].(map[string]any)["w_mm"] != 105.0 {
		t.Errorf("resolved size = %v", res["size"])
	}

	w, out = f.do(t, http.MethodPost, "/cardgen/templates/"+itoaCardgen(action)+"/versions/1/restore", "")
	if w.Code != http.StatusCreated || out["version"].(map[string]any)["version_no"] != 3.0 || out["version"].(map[string]any)["note"] != "restored from v1" {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	w, out = f.do(t, http.MethodGet, "/cardgen/templates/"+itoaCardgen(action)+"/versions", "")
	if w.Code != http.StatusOK || len(out["versions"].([]any)) != 3 || out["current_version_no"] != 3.0 {
		t.Fatalf("versions: %d %s", w.Code, w.Body.String())
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/templates/"+itoaCardgen(action)+"/versions/9/restore", ""); w.Code != http.StatusNotFound {
		t.Errorf("restore v9: %d", w.Code)
	}
}

func TestCardTemplates_SpecRefusalNamesElementAndProp(t *testing.T) {
	f := newCardTemplateFixture(t)
	w, out := f.do(t, http.MethodPost, "/cardgen/templates", `{"slug": "x", "name": "x", "spec": {"elements": {"title": {"onload": "alert(1)"}}}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", w.Code, w.Body.String())
	}
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, `"title"`) || !strings.Contains(msg, `"onload"`) {
		t.Errorf("message does not name element and prop: %s", msg)
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/templates", `{"slug": "Bad Slug", "name": "x", "spec": {}}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad slug: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/templates", `{"slug": "x", "name": "x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing spec: %d", w.Code)
	}
}

func TestCardTemplates_SlugConflictReparentCycleAndDeleteRefusal(t *testing.T) {
	f := newCardTemplateFixture(t)
	base := f.mustCreate(t, "base", nil, `{}`)
	child := f.mustCreate(t, "child", &base, `{}`)
	if w, _ := f.do(t, http.MethodPost, "/cardgen/templates", `{"slug": "base", "name": "again", "spec": {}}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate slug: %d", w.Code)
	}
	// base extending child would be a cycle.
	w, _ := f.do(t, http.MethodPut, "/cardgen/templates/"+itoaCardgen(base), `{"parent_template_id": `+itoaCardgen(child)+`}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("cycle: %d %s", w.Code, w.Body.String())
	}
	// base is extended by child: cannot delete.
	if w, _ := f.do(t, http.MethodDelete, "/cardgen/templates/"+itoaCardgen(base), ""); w.Code != http.StatusConflict {
		t.Errorf("delete referenced: %d", w.Code)
	}
	// A type referencing child blocks child too; clearing the reference frees it.
	w, out := f.do(t, http.MethodPost, "/cardgen/types", `{"slug": "action", "name": "Action", "front_template_id": `+itoaCardgen(child)+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create type: %d %s", w.Code, w.Body.String())
	}
	typeID := uint(out["type"].(map[string]any)["id"].(float64))
	if w, _ := f.do(t, http.MethodDelete, "/cardgen/templates/"+itoaCardgen(child), ""); w.Code != http.StatusConflict {
		t.Errorf("delete type-referenced: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodPut, "/cardgen/types/"+itoaCardgen(typeID), `{"front_template_id": `+itoaCardgen(base)+`, "name": "Action!"}`); w.Code != http.StatusOK {
		t.Errorf("update type: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodDelete, "/cardgen/templates/"+itoaCardgen(child), ""); w.Code != http.StatusOK {
		t.Errorf("delete freed child: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodGet, "/cardgen/templates/"+itoaCardgen(child), ""); w.Code != http.StatusNotFound {
		t.Errorf("get deleted: %d", w.Code)
	}
	w, out = f.do(t, http.MethodGet, "/cardgen/types", "")
	if w.Code != http.StatusOK || len(out["types"].([]any)) != 1 || out["types"].([]any)[0].(map[string]any)["name"] != "Action!" {
		t.Errorf("types: %d %s", w.Code, w.Body.String())
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/types", `{"slug": "action", "name": "dup"}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate type slug: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/types", `{"slug": "life", "name": "Life", "front_template_id": 999}`); w.Code != http.StatusBadRequest {
		t.Errorf("type with missing template: %d", w.Code)
	}
}

func itoaCardgen(v uint) string { return strconv.FormatUint(uint64(v), 10) }
