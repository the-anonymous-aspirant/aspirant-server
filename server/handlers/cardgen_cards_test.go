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

type cardFixture struct {
	db *gorm.DB
	r  *gin.Engine
}

func newCardFixture(t *testing.T) *cardFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&data_models.CardType{}, &data_models.CardTemplate{}, &data_models.CardTemplateVersion{}, &data_models.Card{}, &data_models.CardVersion{})
	data_models.SeedCardgenTemplates(db)
	t.Cleanup(func() { db.Close() })
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("role", "Admin")
		c.Set("user_id", uint(7))
		c.Next()
	})
	g := r.Group("/cardgen")
	g.GET("/cards", ListCardsHandler)
	g.POST("/cards", CreateCardHandler)
	g.GET("/cards/:id", GetCardHandler)
	g.PUT("/cards/:id", UpdateCardHandler)
	g.DELETE("/cards/:id", DeleteCardHandler)
	g.GET("/cards/:id/versions", ListCardVersionsHandler)
	g.POST("/cards/:id/versions", CreateCardVersionHandler)
	g.POST("/cards/:id/versions/:n/restore", RestoreCardVersionHandler)
	g.GET("/cards/:id/resolved", ResolveCardHandler)
	return &cardFixture{db: db, r: r}
}

func (f *cardFixture) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *cardFixture) typeID(t *testing.T, slug string) uint {
	t.Helper()
	var ct data_models.CardType
	if err := f.db.Where("slug = ?", slug).First(&ct).Error; err != nil {
		t.Fatal(err)
	}
	return ct.ID
}

func TestCards_CreateVersionResolveRestoreFilter(t *testing.T) {
	f := newCardFixture(t)
	action := f.typeID(t, "action")
	life := f.typeID(t, "life")
	w, out := f.do(t, http.MethodPost, "/cardgen/cards", `{"slug": "smooth_operator", "name": "Smooth operator", "card_type_id": `+u(action)+`, "data": {"values": {"title": {"text": "SMOOTH OPERATOR"}, "body": {"text": "SELECT A PLAYER"}}}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := uint(out["card"].(map[string]any)["id"].(float64))
	if out["card"].(map[string]any)["current_version_no"] != 1.0 || out["card"].(map[string]any)["data"] == nil {
		t.Errorf("create response = %v", out["card"])
	}
	w, _ = f.do(t, http.MethodPost, "/cardgen/cards", `{"slug": "fatigue", "name": "Fatigue", "card_type_id": `+u(life)+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create without data: %d %s", w.Code, w.Body.String())
	}

	w, out = f.do(t, http.MethodGet, "/cardgen/cards?type_id="+u(action), "")
	if w.Code != http.StatusOK || len(out["cards"].([]any)) != 1 {
		t.Errorf("filter: %d %s", w.Code, w.Body.String())
	}
	w, out = f.do(t, http.MethodGet, "/cardgen/cards", "")
	if len(out["cards"].([]any)) != 2 {
		t.Errorf("list all: %s", w.Body.String())
	}

	w, out = f.do(t, http.MethodGet, "/cardgen/cards/"+u(id)+"/resolved", "")
	if w.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", w.Code, w.Body.String())
	}
	res := out["resolved"].(map[string]any)
	front := res["front"].(map[string]any)
	chain := front["chain"].([]any)
	if chain[len(chain)-1] != "card" || res["back"] == nil {
		t.Errorf("resolved = chain %v back %v", chain, res["back"] != nil)
	}
	var title map[string]any
	for _, e := range front["elements"].([]any) {
		if e.(map[string]any)["name"] == "title" {
			title = e.(map[string]any)
		}
	}
	if title["props"].(map[string]any)["text"] != "SMOOTH OPERATOR" || title["provenance"].(map[string]any)["text"] != "card" {
		t.Errorf("resolved title = %v", title)
	}

	w, out = f.do(t, http.MethodPost, "/cardgen/cards/"+u(id)+"/versions", `{"data": {"values": {"title": {"text": "SMOOTHER"}}}, "note": "rename"}`)
	if w.Code != http.StatusCreated || out["version"].(map[string]any)["version_no"] != 2.0 {
		t.Fatalf("v2: %d %s", w.Code, w.Body.String())
	}
	w, out = f.do(t, http.MethodPost, "/cardgen/cards/"+u(id)+"/versions/1/restore", "")
	if w.Code != http.StatusCreated || out["version"].(map[string]any)["version_no"] != 3.0 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	w, out = f.do(t, http.MethodGet, "/cardgen/cards/"+u(id)+"/versions", "")
	if len(out["versions"].([]any)) != 3 || out["current_version_no"] != 3.0 {
		t.Errorf("versions: %s", w.Body.String())
	}
	w, out = f.do(t, http.MethodPost, "/cardgen/cards/"+u(id)+"/versions", `{"data": {"values": {"title": {"onclick": "x"}}}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(out["error"].(map[string]any)["message"].(string), `"onclick"`) {
		t.Errorf("bad data: %d %s", w.Code, w.Body.String())
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/cards", `{"slug": "smooth_operator", "name": "dup", "card_type_id": `+u(action)+`}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate slug: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodPost, "/cardgen/cards", `{"slug": "x", "name": "x", "card_type_id": 999}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad type: %d", w.Code)
	}
	w, out = f.do(t, http.MethodPut, "/cardgen/cards/"+u(id), `{"name": "Smooth Operator!", "card_type_id": `+u(life)+`}`)
	if w.Code != http.StatusOK || out["card"].(map[string]any)["name"] != "Smooth Operator!" || out["card"].(map[string]any)["card_type_id"] != float64(life) {
		t.Errorf("update: %d %s", w.Code, w.Body.String())
	}
	if w, _ := f.do(t, http.MethodDelete, "/cardgen/cards/"+u(id), ""); w.Code != http.StatusOK {
		t.Errorf("delete: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodGet, "/cardgen/cards/"+u(id), ""); w.Code != http.StatusNotFound {
		t.Errorf("get deleted: %d", w.Code)
	}
}

func u(v uint) string { return strconv.FormatUint(uint64(v), 10) }
