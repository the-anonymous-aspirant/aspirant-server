package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// Invented words only: this repository is public (system_3 #12582).
var christmasTestWords = []string{"Alpha", "Bravo two", "charlie", "Delta", "Echo", "Fox trot", "Golf"}

func christmasTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	if err := db.AutoMigrate(&data_models.ChristmasCard{}, &data_models.ChristmasSolve{}, &data_models.ChristmasSetting{}).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// christmasTestRouter registers the handlers as routes.go does, with the
// session user set inline (the Admin gate is the gate test's).
func christmasTestRouter(db *gorm.DB, userID uint) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", userID)
		c.Set("role", "Admin")
		c.Next()
	})
	g := r.Group("/christmas")
	g.GET("/play", GetChristmasPlayHandler)
	g.GET("/cards/:n/picture", GetChristmasPictureHandler)
	g.POST("/cards/:n/answer", PostChristmasAnswerHandler)
	g.GET("/control", GetChristmasControlHandler)
	g.GET("/control/cards/:n/picture", GetChristmasControlPictureHandler)
	g.PUT("/control/cards/:n/word", PutChristmasWordHandler)
	g.PUT("/control/cards/:n/picture", PutChristmasPictureHandler)
	g.PUT("/control/sentence", PutChristmasSentenceHandler)
	g.PUT("/control/opens", PutChristmasOpensHandler)
	g.POST("/control/reset", PostChristmasResetHandler)
	return r
}

func christmasDo(t *testing.T, r *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func christmasPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	img.Set(1, 1, color.Gray{Y: 10})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func christmasUpload(t *testing.T, r *gin.Engine, n int, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("picture", "card.png")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest(http.MethodPut, "/christmas/control/cards/"+strconv.Itoa(n)+"/picture", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// christmasSetUp stores all seven cards and the sentence through Control.
func christmasSetUp(t *testing.T, r *gin.Engine) {
	t.Helper()
	for i, word := range christmasTestWords {
		n := i + 1
		if w := christmasUpload(t, r, n, christmasPNG(t, 30+n, 20)); w.Code != http.StatusOK {
			t.Fatalf("upload %d: %d %s", n, w.Code, w.Body.String())
		}
		if w := christmasDo(t, r, http.MethodPut, "/christmas/control/cards/"+strconv.Itoa(n)+"/word", gin.H{"word": word}); w.Code != http.StatusOK {
			t.Fatalf("word %d: %d %s", n, w.Code, w.Body.String())
		}
	}
	if w := christmasDo(t, r, http.MethodPut, "/christmas/control/sentence", gin.H{"sentence": "An invented closing line."}); w.Code != http.StatusOK {
		t.Fatalf("sentence: %d %s", w.Code, w.Body.String())
	}
}

func christmasAnswer(t *testing.T, r *gin.Engine, n int, answer string) map[string]interface{} {
	t.Helper()
	w := christmasDo(t, r, http.MethodPost, "/christmas/cards/"+strconv.Itoa(n)+"/answer", gin.H{"answer": answer})
	if w.Code != http.StatusOK {
		t.Fatalf("answer %d: %d %s", n, w.Code, w.Body.String())
	}
	var out map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func christmasPlay(t *testing.T, r *gin.Engine) map[string]interface{} {
	t.Helper()
	w := christmasDo(t, r, http.MethodGet, "/christmas/play", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("play: %d %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestNormalizeChristmasWord(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"blue kite", "  Blue   Kite ", true},
		{"Lantern", "lantern ", true},
		{"up the hill", "up the\thill", true},
		{"blue kite", "bluekite", false},
		{"maple", "mapl", false},
	} {
		got := data_models.NormalizeChristmasWord(tc.a) == data_models.NormalizeChristmasWord(tc.b)
		if got != tc.same {
			t.Errorf("%q vs %q: same=%v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// The whole journey: card 1, a wrong word, every right word in a loose
// spelling, the closing sentence.
func TestChristmasPlay_Journey(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)

	play := christmasPlay(t, r)
	if play["current"] != float64(1) || play["done"] != false {
		t.Fatalf("start: %v", play)
	}
	if got := christmasAnswer(t, r, 1, "Bravo"); got["correct"] != false {
		t.Fatalf("a wrong word: %v", got)
	}
	if christmasPlay(t, r)["current"] != float64(1) {
		t.Fatal("a wrong word moved the player")
	}
	for i, word := range christmasTestWords {
		loose := "  " + strings.ToUpper(strings.ReplaceAll(word, " ", "   ")) + " "
		got := christmasAnswer(t, r, i+1, loose)
		if got["correct"] != true || got["done"] != (i+1 == 7) {
			t.Fatalf("card %d with %q: %v", i+1, loose, got)
		}
	}
	play = christmasPlay(t, r)
	if play["done"] != true || play["sentence"] != "An invented closing line." {
		t.Fatalf("end: %v", play)
	}
	if cards, _ := play["cards"].([]interface{}); len(cards) != 7 {
		t.Fatalf("end cards: %v", play["cards"])
	}
}

// Play never carries a word, and the sentence only once every card is solved.
func TestChristmasPlay_NeverReturnsAWord(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	for i := range christmasTestWords {
		body := christmasDo(t, r, http.MethodGet, "/christmas/play", nil).Body.String()
		for _, word := range christmasTestWords {
			if strings.Contains(strings.ToLower(body), strings.ToLower(word)) {
				t.Fatalf("play on card %d carries %q: %s", i+1, word, body)
			}
		}
		if strings.Contains(body, "closing line") {
			t.Fatalf("play on card %d carries the sentence: %s", i+1, body)
		}
		christmasAnswer(t, r, i+1, christmasTestWords[i])
	}
}

// A picture past the current card is a 404, like a card that does not exist;
// Control's own picture route is not held back.
func TestChristmasPicture_LockedIsNotFound(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	for path, want := range map[string]int{
		"/christmas/cards/1/picture":         http.StatusOK,
		"/christmas/cards/2/picture":         http.StatusNotFound,
		"/christmas/cards/7/picture":         http.StatusNotFound,
		"/christmas/cards/8/picture":         http.StatusNotFound,
		"/christmas/cards/0/picture":         http.StatusNotFound,
		"/christmas/control/cards/7/picture": http.StatusOK,
	} {
		w := christmasDo(t, r, http.MethodGet, path, nil)
		if w.Code != want {
			t.Errorf("%s: got %d, want %d", path, w.Code, want)
		}
		if w.Code == http.StatusOK {
			if ct := w.Header().Get("Content-Type"); ct != "image/png" {
				t.Errorf("%s: Content-Type %q", path, ct)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Errorf("%s: headers %v", path, w.Header())
			}
		}
	}
	christmasAnswer(t, r, 1, "alpha")
	if w := christmasDo(t, r, http.MethodGet, "/christmas/cards/2/picture", nil); w.Code != http.StatusOK {
		t.Errorf("card 2 after solving 1: %d", w.Code)
	}
}

// An answer for any card but the current one is refused, and moves nothing.
func TestChristmasAnswer_OnlyTheCurrentCard(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	w := christmasDo(t, r, http.MethodPost, "/christmas/cards/2/answer", gin.H{"answer": "Bravo two"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not_current") {
		t.Fatalf("card 2 first: %d %s", w.Code, w.Body.String())
	}
	if christmasPlay(t, r)["current"] != float64(1) {
		t.Fatal("an answer for a later card moved the player")
	}
	w = christmasDo(t, r, http.MethodPost, "/christmas/cards/1/answer", gin.H{"answer": strings.Repeat("a", 21)})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"limit":20`) {
		t.Fatalf("21 characters: %d %s", w.Code, w.Body.String())
	}
}

// A card with no word or no picture stops Play in front of it.
func TestChristmasAnswer_CardNotReady(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasUpload(t, r, 1, christmasPNG(t, 10, 10))
	play := christmasPlay(t, r)
	if card, _ := play["card"].(map[string]interface{}); card["ready"] != false {
		t.Fatalf("a card with no word reads ready: %v", play)
	}
	w := christmasDo(t, r, http.MethodPost, "/christmas/cards/1/answer", gin.H{"answer": ""})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not_ready") {
		t.Fatalf("answer on a card with no word: %d %s", w.Code, w.Body.String())
	}
}

// Progress is per user.
func TestChristmasProgress_PerUser(t *testing.T) {
	db := christmasTestDB(t)
	one := christmasTestRouter(db, 1)
	christmasSetUp(t, one)
	christmasAnswer(t, one, 1, "alpha")
	if got := christmasPlay(t, christmasTestRouter(db, 2))["current"]; got != float64(1) {
		t.Fatalf("user 2 starts on %v", got)
	}
}

// Control shows the words as typed and the states; going back locks the cards
// from that one on and clears their times; a re-worded solved card stays solved.
func TestChristmasControl_StatesResetAndWords(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	for i := 0; i < 4; i++ {
		christmasAnswer(t, r, i+1, christmasTestWords[i])
	}
	type ctl struct {
		Solved int `json:"solved"`
		Cards  []struct {
			Number   int     `json:"number"`
			Word     string  `json:"word"`
			State    string  `json:"state"`
			SolvedAt *string `json:"solved_at"`
		} `json:"cards"`
	}
	read := func(w *httptest.ResponseRecorder) ctl {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("control: %d %s", w.Code, w.Body.String())
		}
		var out ctl
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	got := read(christmasDo(t, r, http.MethodGet, "/christmas/control", nil))
	if got.Cards[1].Word != "Bravo two" || got.Cards[3].State != "solved" || got.Cards[4].State != "current" || got.Cards[5].State != "locked" || got.Cards[3].SolvedAt == nil {
		t.Fatalf("control: %+v", got)
	}
	got = read(christmasDo(t, r, http.MethodPut, "/christmas/control/cards/2/word", gin.H{"word": "Bravo three"}))
	if got.Cards[1].Word != "Bravo three" || got.Cards[1].State != "solved" {
		t.Fatalf("re-worded solved card: %+v", got.Cards[1])
	}
	got = read(christmasDo(t, r, http.MethodPost, "/christmas/control/reset", gin.H{"to": 3}))
	if got.Solved != 2 || got.Cards[2].State != "current" || got.Cards[3].SolvedAt != nil {
		t.Fatalf("after going back to 3: %+v", got)
	}
	if christmasPlay(t, r)["current"] != float64(3) {
		t.Fatal("play is not on card 3 after going back")
	}
}

func TestChristmasControl_Refusals(t *testing.T) {
	r := christmasTestRouter(christmasTestDB(t), 1)
	for _, tc := range []struct {
		name, method, path string
		body               interface{}
		code               string
	}{
		{"an empty word", http.MethodPut, "/christmas/control/cards/1/word", gin.H{"word": "   "}, "required"},
		{"a 21-character word", http.MethodPut, "/christmas/control/cards/1/word", gin.H{"word": strings.Repeat("x", 21)}, "too_long"},
		{"a word with a direction mark", http.MethodPut, "/christmas/control/cards/1/word", gin.H{"word": "a‮b"}, "hidden_characters"},
		{"card 8", http.MethodPut, "/christmas/control/cards/8/word", gin.H{"word": "x"}, "not_found"},
		{"going back to 0", http.MethodPost, "/christmas/control/reset", gin.H{"to": 0}, "invalid_choice"},
		{"an empty sentence", http.MethodPut, "/christmas/control/sentence", gin.H{"sentence": ""}, "required"},
	} {
		w := christmasDo(t, r, tc.method, tc.path, tc.body)
		if w.Code < 400 || w.Code >= 500 || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if w := christmasUpload(t, r, 1, []byte("GIF89a not a picture")); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unsupported_file_type") {
		t.Errorf("a GIF: %d %s", w.Code, w.Body.String())
	}
}

// christmasAt sets the server's clock for one test.
func christmasAt(t *testing.T, now time.Time) {
	t.Helper()
	was := christmasNow
	christmasNow = func() time.Time { return now }
	t.Cleanup(func() { christmasNow = was })
}

func christmasOpens(t *testing.T, r *gin.Engine, opens []interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return christmasDo(t, r, http.MethodPut, "/christmas/control/opens", gin.H{"opens_at": opens})
}

// #12780: a card waits for its opening time on the server's clock. Before it,
// Play shows the card as not open, with the time and the server's now; its
// picture is a 404 and its word is refused. At the time, all three open.
func TestChristmasOpens_ACardWaitsForItsTime(t *testing.T) {
	t0 := time.Date(2026, 12, 19, 22, 0, 0, 0, time.UTC)
	christmasAt(t, t0)
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	if w := christmasOpens(t, r, []interface{}{nil, "2026-12-20T00:00:00+01:00", nil, nil, nil, nil, nil}); w.Code != http.StatusOK {
		t.Fatalf("opens: %d %s", w.Code, w.Body.String())
	}
	christmasAnswer(t, r, 1, "alpha")

	play := christmasPlay(t, r)
	card, _ := play["card"].(map[string]interface{})
	if play["current"] != float64(2) || card["open"] != false || card["opens_at"] != "2026-12-19T23:00:00Z" || play["now"] != "2026-12-19T22:00:00Z" {
		t.Fatalf("before card 2 opens: %v", play)
	}
	if w := christmasDo(t, r, http.MethodGet, "/christmas/cards/2/picture", nil); w.Code != http.StatusNotFound {
		t.Errorf("card 2's picture before it opens: %d, want 404", w.Code)
	}
	w := christmasDo(t, r, http.MethodPost, "/christmas/cards/2/answer", gin.H{"answer": "Bravo two"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not_open") {
		t.Fatalf("card 2's word before it opens: %d %s", w.Code, w.Body.String())
	}
	if christmasPlay(t, r)["current"] != float64(2) {
		t.Fatal("a refused word moved the player")
	}
	if w := christmasDo(t, r, http.MethodGet, "/christmas/cards/1/picture", nil); w.Code != http.StatusOK {
		t.Errorf("solved card 1's picture: %d, want 200", w.Code)
	}

	christmasAt(t, t0.Add(time.Hour)) // 2026-12-19T23:00Z: midnight in Berlin
	card, _ = christmasPlay(t, r)["card"].(map[string]interface{})
	if card["open"] != true {
		t.Fatalf("at its time: %v", card)
	}
	if w := christmasDo(t, r, http.MethodGet, "/christmas/cards/2/picture", nil); w.Code != http.StatusOK {
		t.Errorf("card 2's picture at its time: %d, want 200", w.Code)
	}
	if got := christmasAnswer(t, r, 2, "bravo two"); got["correct"] != true {
		t.Fatalf("card 2's word at its time: %v", got)
	}
}

// Card 1 can wait too, and a solved card stays shown when its time moves later.
func TestChristmasOpens_CardOneAndASolvedCard(t *testing.T) {
	christmasAt(t, time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC))
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	christmasAnswer(t, r, 1, "alpha")
	later := "2026-12-25T00:00:00+01:00"
	if w := christmasOpens(t, r, []interface{}{later, later, nil, nil, nil, nil, nil}); w.Code != http.StatusOK {
		t.Fatalf("opens: %d %s", w.Code, w.Body.String())
	}
	if w := christmasDo(t, r, http.MethodGet, "/christmas/cards/1/picture", nil); w.Code != http.StatusOK {
		t.Errorf("solved card 1 after its time moved later: %d, want 200", w.Code)
	}
	if w := christmasDo(t, r, http.MethodPost, "/christmas/control/reset", gin.H{"to": 1}); w.Code != http.StatusOK {
		t.Fatalf("reset: %d", w.Code)
	}
	card, _ := christmasPlay(t, r)["card"].(map[string]interface{})
	if card["number"] != float64(1) || card["open"] != false {
		t.Fatalf("card 1 before its time: %v", card)
	}
	if w := christmasDo(t, r, http.MethodGet, "/christmas/cards/1/picture", nil); w.Code != http.StatusNotFound {
		t.Errorf("card 1's picture before its time: %d, want 404", w.Code)
	}
}

// Control stores all seven times at once, null clears one, and a bad request
// stores nothing.
func TestChristmasOpens_Control(t *testing.T) {
	christmasAt(t, time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC))
	r := christmasTestRouter(christmasTestDB(t), 1)
	christmasSetUp(t, r)
	week := []interface{}{}
	for d := 19; d <= 25; d++ {
		week = append(week, fmt.Sprintf("2026-12-%02dT00:00:00+01:00", d))
	}
	w := christmasOpens(t, r, week)
	if w.Code != http.StatusOK {
		t.Fatalf("a week: %d %s", w.Code, w.Body.String())
	}
	var ctl struct {
		Now   string `json:"now"`
		Cards []struct {
			Number  int     `json:"number"`
			OpensAt *string `json:"opens_at"`
		} `json:"cards"`
	}
	json.Unmarshal(w.Body.Bytes(), &ctl)
	if ctl.Now != "2026-12-01T12:00:00Z" || len(ctl.Cards) != 7 || ctl.Cards[0].OpensAt == nil || *ctl.Cards[0].OpensAt != "2026-12-18T23:00:00Z" ||
		ctl.Cards[6].OpensAt == nil || *ctl.Cards[6].OpensAt != "2026-12-24T23:00:00Z" {
		t.Fatalf("control after a week: %s", w.Body.String())
	}

	for name, body := range map[string]interface{}{
		"six times":  week[:6],
		"eight":      append(append([]interface{}{}, week...), nil),
		"not a time": []interface{}{nil, nil, "Dec 20", nil, nil, nil, nil},
		"no zone":    []interface{}{nil, nil, nil, "2026-12-22T00:00:00", nil, nil, nil},
	} {
		if w := christmasOpens(t, r, body.([]interface{})); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, w.Code)
		}
	}
	w = christmasDo(t, r, http.MethodGet, "/christmas/control", nil)
	json.Unmarshal(w.Body.Bytes(), &ctl)
	if ctl.Cards[2].OpensAt == nil || *ctl.Cards[2].OpensAt != "2026-12-20T23:00:00Z" {
		t.Fatalf("a refused request changed a time: %s", w.Body.String())
	}
	w = christmasOpens(t, r, []interface{}{nil, nil, nil, nil, nil, nil, nil})
	json.Unmarshal(w.Body.Bytes(), &ctl)
	for _, k := range ctl.Cards {
		if k.OpensAt != nil {
			t.Fatalf("null did not clear card %d: %s", k.Number, w.Body.String())
		}
	}
}
