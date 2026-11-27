package handlers

// The Christmas cards (system_3 #12582): an admin-only picture puzzle. Every
// route is in the Admin group (routes.go, /christmas), walked by
// TestChristmasRoutes_OnlyAdminPassesTheGate.
//
// Two halves, and the line between them is the point:
//   - Play (/christmas/play, /christmas/cards/:n/...) never returns a word.
//     A picture past the player's current card is a 404, the same answer as
//     a card that does not exist, and the closing sentence comes only once
//     the last card is solved.
//   - Control (/christmas/control/...) returns the words, stores the
//     pictures, the words and the sentence, and moves a player back.

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // DecodeConfig for a stored JPEG picture
	_ "image/png"  // DecodeConfig for a stored PNG picture
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

const (
	christmasPictureField   = "picture"
	christmasFormSlack      = 64 << 10
	christmasMaxPictureSide = 8000
)

// christmasRefuse answers a 4xx with a code the client words itself.
func christmasRefuse(c *gin.Context, status int, code, field string, limit int64, msg string) {
	c.JSON(status, ErrorResponse{Error: ErrorDetail{Code: code, Message: msg, Field: field, Limit: limit}})
}

// christmasNumber reads :n, answering 404 for anything that is not a card.
func christmasNumber(c *gin.Context) (int, bool) {
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n < 1 || n > data_models.ChristmasCardCount {
		RespondWithError(c, http.StatusNotFound, "No such card")
		return 0, false
	}
	return n, true
}

// christmasSolved is how many cards the user has solved in order from card 1.
// A solve past a gap (left by a Control reset of an earlier card) does not
// count, so the player is always on the first unsolved card.
func christmasSolved(db *gorm.DB, userID uint) (int, map[int]time.Time, error) {
	var solves []data_models.ChristmasSolve
	if err := db.Where("user_id = ?", userID).Find(&solves).Error; err != nil {
		return 0, nil, err
	}
	at := make(map[int]time.Time, len(solves))
	for _, s := range solves {
		at[s.Number] = s.SolvedAt
	}
	n := 0
	for n < data_models.ChristmasCardCount {
		if _, ok := at[n+1]; !ok {
			break
		}
		n++
	}
	return n, at, nil
}

// christmasCards loads the stored cards by number, without their bytes (a
// one-byte stand-in marks a stored picture, so Ready() reads the same). Absent
// rows are absent.
func christmasCards(db *gorm.DB) (map[int]data_models.ChristmasCard, error) {
	var rows []christmasCardRow
	err := db.Model(&data_models.ChristmasCard{}).
		Select("number, word, media_type, width, height, updated_at, (picture IS NOT NULL AND length(picture) > 0) AS has_picture").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[int]data_models.ChristmasCard, len(rows))
	for _, r := range rows {
		card := r.ChristmasCard
		if r.HasPicture {
			card.Picture = []byte{0}
		}
		out[card.Number] = card
	}
	return out, nil
}

type christmasCardRow struct {
	data_models.ChristmasCard
	HasPicture bool
}

func christmasSentence(db *gorm.DB) (string, error) {
	var s data_models.ChristmasSetting
	err := db.Where("key = ?", data_models.ChristmasSentenceKey).First(&s).Error
	if gorm.IsRecordNotFoundError(err) {
		return "", nil
	}
	return s.Value, err
}

type christmasPlayCard struct {
	Number int  `json:"number"`
	Ready  bool `json:"ready"`
	Width  int  `json:"width"`
	Height int  `json:"height"`
}

// GetChristmasPlayHandler handles GET /christmas/play: the card the player is
// on, or, once every card is solved, the closing sentence and the cards.
func GetChristmasPlayHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID := c.MustGet("user_id").(uint)
	solved, _, err := christmasSolved(db, userID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the progress")
		return
	}
	cards, err := christmasCards(db)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the cards")
		return
	}
	total := data_models.ChristmasCardCount
	if solved == total {
		sentence, err := christmasSentence(db)
		if err != nil {
			RespondWithError(c, http.StatusInternalServerError, "Could not read the sentence")
			return
		}
		list := make([]christmasPlayCard, 0, total)
		for n := 1; n <= total; n++ {
			k := cards[n]
			list = append(list, christmasPlayCard{Number: n, Ready: k.Ready(), Width: k.Width, Height: k.Height})
		}
		c.JSON(http.StatusOK, gin.H{"total": total, "done": true, "sentence": sentence, "cards": list})
		return
	}
	n := solved + 1
	k := cards[n]
	c.JSON(http.StatusOK, gin.H{"total": total, "done": false, "current": n,
		"card": christmasPlayCard{Number: n, Ready: k.Ready(), Width: k.Width, Height: k.Height}})
}

// serveChristmasPicture writes a card's stored bytes, or 404.
func serveChristmasPicture(c *gin.Context, db *gorm.DB, n int) {
	var card data_models.ChristmasCard
	if err := db.Where("number = ?", n).First(&card).Error; err != nil || len(card.Picture) == 0 {
		RespondWithError(c, http.StatusNotFound, "No such card")
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", "inline")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	// A picture can be replaced on Control, and is nobody's to keep.
	c.Header("Cache-Control", "private, no-store")
	c.Data(http.StatusOK, card.MediaType, card.Picture)
}

// GetChristmasPictureHandler handles GET /christmas/cards/:n/picture: a card
// up to the player's current one, else 404 (not 403: a locked card must not
// read differently from a missing one).
func GetChristmasPictureHandler(c *gin.Context) {
	n, ok := christmasNumber(c)
	if !ok {
		return
	}
	db := c.MustGet("db").(*gorm.DB)
	solved, _, err := christmasSolved(db, c.MustGet("user_id").(uint))
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the progress")
		return
	}
	if n > solved+1 {
		RespondWithError(c, http.StatusNotFound, "No such card")
		return
	}
	serveChristmasPicture(c, db, n)
}

type christmasAnswerRequest struct {
	Answer string `json:"answer"`
}

// PostChristmasAnswerHandler handles POST /christmas/cards/:n/answer. The
// answer is only ever {correct}; a right word records the solve.
func PostChristmasAnswerHandler(c *gin.Context) {
	n, ok := christmasNumber(c)
	if !ok {
		return
	}
	var req christmasAnswerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		christmasRefuse(c, http.StatusBadRequest, "invalid_request", "answer", 0, "The answer could not be read.")
		return
	}
	if utf8.RuneCountInString(req.Answer) > data_models.MaxChristmasWordRunes {
		christmasRefuse(c, http.StatusBadRequest, codeTooLong, "answer", data_models.MaxChristmasWordRunes,
			fmt.Sprintf("An answer is at most %d characters.", data_models.MaxChristmasWordRunes))
		return
	}
	db := c.MustGet("db").(*gorm.DB)
	userID := c.MustGet("user_id").(uint)
	solved, _, err := christmasSolved(db, userID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the progress")
		return
	}
	if n != solved+1 {
		// Another tab moved on, or Control moved the player back.
		christmasRefuse(c, http.StatusConflict, "not_current", "", 0, "This is not the current card.")
		return
	}
	var card data_models.ChristmasCard
	err = db.Where("number = ?", n).First(&card).Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the card")
		return
	}
	if err != nil || !card.Ready() {
		christmasRefuse(c, http.StatusConflict, "not_ready", "", 0, "This card has no picture or no word yet.")
		return
	}
	if data_models.NormalizeChristmasWord(req.Answer) != data_models.NormalizeChristmasWord(card.Word) {
		c.JSON(http.StatusOK, gin.H{"correct": false})
		return
	}
	solve := data_models.ChristmasSolve{UserID: userID, Number: n, SolvedAt: time.Now().UTC()}
	if err := db.Where(data_models.ChristmasSolve{UserID: userID, Number: n}).FirstOrCreate(&solve).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not record the answer")
		return
	}
	c.JSON(http.StatusOK, gin.H{"correct": true, "done": n == data_models.ChristmasCardCount})
}

type christmasControlCard struct {
	Number     int        `json:"number"`
	Word       string     `json:"word"`
	HasPicture bool       `json:"has_picture"`
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	State      string     `json:"state"` // solved | current | locked
	SolvedAt   *time.Time `json:"solved_at"`
}

func christmasControl(c *gin.Context, db *gorm.DB, userID uint) {
	solved, at, err := christmasSolved(db, userID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the progress")
		return
	}
	cards, err := christmasCards(db)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the cards")
		return
	}
	sentence, err := christmasSentence(db)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not read the sentence")
		return
	}
	list := make([]christmasControlCard, 0, data_models.ChristmasCardCount)
	for n := 1; n <= data_models.ChristmasCardCount; n++ {
		k := cards[n]
		row := christmasControlCard{Number: n, Word: k.Word, HasPicture: len(k.Picture) > 0, Width: k.Width, Height: k.Height, State: "locked"}
		switch {
		case n <= solved:
			row.State = "solved"
			t := at[n]
			row.SolvedAt = &t
		case n == solved+1:
			row.State = "current"
		}
		list = append(list, row)
	}
	c.JSON(http.StatusOK, gin.H{"total": data_models.ChristmasCardCount, "solved": solved, "cards": list, "sentence": sentence})
}

// GetChristmasControlHandler handles GET /christmas/control: every card with
// its word, and the requesting admin's own progress.
func GetChristmasControlHandler(c *gin.Context) {
	christmasControl(c, c.MustGet("db").(*gorm.DB), c.MustGet("user_id").(uint))
}

// GetChristmasControlPictureHandler handles GET
// /christmas/control/cards/:n/picture: any stored picture, for Control's table.
func GetChristmasControlPictureHandler(c *gin.Context) {
	n, ok := christmasNumber(c)
	if !ok {
		return
	}
	serveChristmasPicture(c, c.MustGet("db").(*gorm.DB), n)
}

// christmasCheckText refuses an empty, over-long or control-character text.
func christmasCheckText(c *gin.Context, field, s string, limit int, allowNewlines bool) bool {
	if strings.TrimSpace(s) == "" {
		christmasRefuse(c, http.StatusBadRequest, "required", field, 0, "This cannot be empty.")
		return false
	}
	if utf8.RuneCountInString(s) > limit {
		christmasRefuse(c, http.StatusBadRequest, codeTooLong, field, int64(limit), fmt.Sprintf("This is at most %d characters.", limit))
		return false
	}
	for _, r := range s {
		if (unicode.IsControl(r) && !(allowNewlines && r == '\n')) || unicode.Is(unicode.Bidi_Control, r) {
			christmasRefuse(c, http.StatusBadRequest, codeHiddenCharacters, field, 0, "This has a control or a direction character.")
			return false
		}
	}
	return true
}

type christmasWordRequest struct {
	Word string `json:"word"`
}

// PutChristmasWordHandler handles PUT /christmas/control/cards/:n/word. The
// word is kept as typed; matching normalises both sides. A solved card stays
// solved.
func PutChristmasWordHandler(c *gin.Context) {
	n, ok := christmasNumber(c)
	if !ok {
		return
	}
	var req christmasWordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		christmasRefuse(c, http.StatusBadRequest, "invalid_request", "word", 0, "The word could not be read.")
		return
	}
	if !christmasCheckText(c, "word", req.Word, data_models.MaxChristmasWordRunes, false) {
		return
	}
	db := c.MustGet("db").(*gorm.DB)
	card := data_models.ChristmasCard{Number: n}
	if err := db.Where("number = ?", n).Assign(map[string]interface{}{"word": req.Word}).FirstOrCreate(&card).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not save the word")
		return
	}
	christmasControl(c, db, c.MustGet("user_id").(uint))
}

// PutChristmasPictureHandler handles PUT /christmas/control/cards/:n/picture,
// a multipart form with one "picture" file: a PNG or a JPEG.
func PutChristmasPictureHandler(c *gin.Context) {
	n, ok := christmasNumber(c)
	if !ok {
		return
	}
	limit := int64(data_models.MaxChristmasPictureBytes)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+christmasFormSlack)
	file, err := c.FormFile(christmasPictureField)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			christmasRefuse(c, http.StatusBadRequest, "file_too_large", christmasPictureField, limit, "The picture is too large.")
			return
		}
		christmasRefuse(c, http.StatusBadRequest, "invalid_request", christmasPictureField, 0, "No picture was sent.")
		return
	}
	f, err := file.Open()
	if err != nil {
		christmasRefuse(c, http.StatusBadRequest, "invalid_request", christmasPictureField, 0, "The picture could not be read.")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		christmasRefuse(c, http.StatusBadRequest, "file_too_large", christmasPictureField, limit, "The picture is too large.")
		return
	}
	mediaType := http.DetectContentType(data)
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		christmasRefuse(c, http.StatusBadRequest, "unsupported_file_type", christmasPictureField, 0, "The picture must be a PNG or a JPEG.")
		return
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > christmasMaxPictureSide || cfg.Height > christmasMaxPictureSide {
		christmasRefuse(c, http.StatusBadRequest, "unreadable_file", christmasPictureField, christmasMaxPictureSide, "The picture could not be read.")
		return
	}
	db := c.MustGet("db").(*gorm.DB)
	card := data_models.ChristmasCard{Number: n}
	if err := db.Where("number = ?", n).Assign(map[string]interface{}{
		"picture": data, "media_type": mediaType, "width": cfg.Width, "height": cfg.Height,
	}).FirstOrCreate(&card).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not save the picture")
		return
	}
	christmasControl(c, db, c.MustGet("user_id").(uint))
}

type christmasSentenceRequest struct {
	Sentence string `json:"sentence"`
}

// PutChristmasSentenceHandler handles PUT /christmas/control/sentence.
func PutChristmasSentenceHandler(c *gin.Context) {
	var req christmasSentenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		christmasRefuse(c, http.StatusBadRequest, "invalid_request", "sentence", 0, "The sentence could not be read.")
		return
	}
	if !christmasCheckText(c, "sentence", req.Sentence, data_models.MaxChristmasSentenceRunes, true) {
		return
	}
	db := c.MustGet("db").(*gorm.DB)
	s := data_models.ChristmasSetting{Key: data_models.ChristmasSentenceKey}
	if err := db.Where("key = ?", s.Key).Assign(map[string]interface{}{"value": req.Sentence}).FirstOrCreate(&s).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not save the sentence")
		return
	}
	christmasControl(c, db, c.MustGet("user_id").(uint))
}

type christmasResetRequest struct {
	To int `json:"to"`
}

// PostChristmasResetHandler handles POST /christmas/control/reset {to}: the
// requesting admin goes back to card `to`; cards from `to` on are locked again
// and their solve times cleared.
func PostChristmasResetHandler(c *gin.Context) {
	var req christmasResetRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.To < 1 || req.To > data_models.ChristmasCardCount {
		christmasRefuse(c, http.StatusBadRequest, "invalid_choice", "to", 0, "Choose a card from 1 to 7.")
		return
	}
	db := c.MustGet("db").(*gorm.DB)
	userID := c.MustGet("user_id").(uint)
	if err := db.Where("user_id = ? AND number >= ?", userID, req.To).Delete(&data_models.ChristmasSolve{}).Error; err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Could not go back")
		return
	}
	christmasControl(c, db, userID)
}
