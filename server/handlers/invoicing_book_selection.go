package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// What the Book counts, and her presets (system_3 #10555-A3, design §3.143
// Decision 8, operator c65429):
//
//	GET    /invoicing/book/selection     the current selection and every preset
//	PUT    /invoicing/book/selection     set it
//	POST   /invoicing/book/presets       save a selection under a name
//	PUT    /invoicing/book/presets/:id   update one of her presets
//	DELETE /invoicing/book/presets/:id   delete one of her presets
//
// "Work only" is built in (id "work_only"): it cannot be updated or deleted.
// Private never counts: no selection or preset may include it. Every write
// records one audit event.

const (
	invoicingEntityBookSelection = "book_selection"
	invoicingEntityBookPreset    = "book_preset"

	maxBookPresetNameRunes = 60
)

type bookPresetView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	data_models.BookCounting
}

type bookSelectionView struct {
	data_models.BookCounting
	Preset  string `json:"preset"`
	Changed bool   `json:"changed"`
}

func workOnlyView() bookPresetView {
	return bookPresetView{ID: data_models.BookPresetWorkOnly, Name: data_models.BookPresetWorkOnlyName, Builtin: true, BookCounting: data_models.BookWorkOnly().Normalised()}
}

func presetView(p data_models.InvoicingBookPreset) bookPresetView {
	return bookPresetView{ID: strconv.FormatUint(uint64(p.ID), 10), Name: p.Name, BookCounting: p.Counting()}
}

// bookCountingBody reads a selection from a request body. Unknown keys and
// missing lists are refused, so a client cannot half-set a selection.
func bookCountingBody(body map[string]json.RawMessage) (data_models.BookCounting, *rowRefusal) {
	var b data_models.BookCounting
	for _, k := range []string{"income", "expense_all", "expense"} {
		if _, ok := body[k]; !ok {
			return b, refuseRow(http.StatusBadRequest, "invalid_field", "%s is required", k)
		}
	}
	if json.Unmarshal(body["income"], &b.Income) != nil || b.Income == nil {
		return b, refuseRow(http.StatusBadRequest, "invalid_field", "income must be a list of payment categories")
	}
	if json.Unmarshal(body["expense_all"], &b.ExpenseAll) != nil {
		return b, refuseRow(http.StatusBadRequest, "invalid_field", "expense_all must be true or false")
	}
	if json.Unmarshal(body["expense"], &b.Expense) != nil || b.Expense == nil {
		return b, refuseRow(http.StatusBadRequest, "invalid_field", "expense must be a list of expense categories")
	}
	return b, nil
}

// checkBookCounting refuses a selection that names a category that does not
// exist for the owner, names one twice, or includes Private.
func checkBookCounting(tx *gorm.DB, owner uint, b data_models.BookCounting) (*rowRefusal, error) {
	seen := map[string]bool{}
	for _, c := range b.Income {
		if !data_models.IsPaymentCategory(c) {
			return refuseRow(http.StatusBadRequest, "invalid_field", "%q is not a payment category", c), nil
		}
		if seen["in:"+c] {
			return refuseRow(http.StatusBadRequest, "invalid_field", "%q is listed twice", c), nil
		}
		seen["in:"+c] = true
	}
	if b.ExpenseAll && len(b.Expense) > 0 {
		return refuseRow(http.StatusBadRequest, "invalid_field", "expense must be empty when expense_all is true"), nil
	}
	for _, c := range b.Expense {
		if strings.EqualFold(c, data_models.ExpenseCategoryPrivate) {
			return refuseRow(http.StatusBadRequest, "private_never_counts", "Private expenses never count"), nil
		}
		if seen["out:"+c] {
			return refuseRow(http.StatusBadRequest, "invalid_field", "%q is listed twice", c), nil
		}
		seen["out:"+c] = true
		ok, err := data_models.IsExpenseCategoryForOwner(tx, owner, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			return refuseRow(http.StatusBadRequest, "invalid_field", "%q is not one of your expense categories", c), nil
		}
	}
	return nil, nil
}

// workOnlyRule refuses expense_all anywhere but Work only (design_agent c66120
// on #10555-A3): a preset, and any selection she changed, counts exactly the
// expense categories she ticked, so a category added later is never counted
// without her tick (D13). Only Work only is a rule.
func workOnlyRule(b data_models.BookCounting, preset bool) *rowRefusal {
	if !b.ExpenseAll {
		return nil
	}
	if preset || !b.Equal(data_models.BookWorkOnly()) {
		return refuseRow(http.StatusBadRequest, "invalid_field", "only Work only counts every expense category; list the categories that count")
	}
	return nil
}

// presetForSelection resolves the preset a selection says it came from:
// Work only, one of her presets, or none. Its counting is returned too.
func presetForSelection(tx *gorm.DB, owner uint, id string) (*data_models.BookCounting, *rowRefusal, error) {
	switch id {
	case "":
		return nil, nil, nil
	case data_models.BookPresetWorkOnly:
		w := data_models.BookWorkOnly()
		return &w, nil, nil
	}
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return nil, refuseRow(http.StatusBadRequest, "invalid_field", "preset must be \"work_only\", one of your presets' ids, or empty"), nil
	}
	p, err := data_models.GetInvoicingBookPreset(tx, owner, uint(n))
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, refuseRow(http.StatusNotFound, "not_found", "preset %d not found", n), nil
	}
	c := p.Counting()
	return &c, nil, nil
}

func readBookSelection(db *gorm.DB, owner uint) (bookSelectionView, []bookPresetView, error) {
	presets := []bookPresetView{workOnlyView()}
	own, err := data_models.ListInvoicingBookPresets(db, owner)
	if err != nil {
		return bookSelectionView{}, nil, err
	}
	for _, p := range own {
		presets = append(presets, presetView(p))
	}
	s, err := data_models.GetInvoicingBookSelection(db, owner)
	if err != nil {
		return bookSelectionView{}, nil, err
	}
	if s == nil {
		return bookSelectionView{BookCounting: data_models.BookWorkOnly().Normalised(), Preset: data_models.BookPresetWorkOnly}, presets, nil
	}
	view := bookSelectionView{BookCounting: s.Counting(), Preset: s.Preset}
	for _, p := range presets {
		if p.ID == s.Preset {
			view.Changed = !p.BookCounting.Equal(view.BookCounting)
		}
	}
	return view, presets, nil
}

// GetInvoicingBookSelectionHandler serves GET /invoicing/book/selection.
func GetInvoicingBookSelectionHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	view, presets, err := readBookSelection(db, invoicingUserID(c))
	if err != nil {
		log.Printf("book selection: read: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading the Book's selection")
		return
	}
	c.JSON(http.StatusOK, gin.H{"selection": view, "presets": presets})
}

// bookWrite runs fn in a transaction under the book lock and answers with the
// fresh selection and presets, or the refusal or error fn returned.
func bookWrite(c *gin.Context, what string, status int, fn func(tx *gorm.DB, owner uint) (*rowRefusal, gin.H, error)) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)
	tx := db.Begin()
	if err := data_models.LockInvoicingBook(tx, owner); err != nil {
		tx.Rollback()
		log.Printf("book %s: lock: %v", what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving the Book's selection")
		return
	}
	refusal, extra, err := fn(tx, owner)
	if err != nil {
		tx.Rollback()
		log.Printf("book %s: %v", what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving the Book's selection")
		return
	}
	if refusal != nil {
		tx.Rollback()
		respondError(c, refusal.status, refusal.code, refusal.message)
		return
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("book %s: commit: %v", what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error saving the Book's selection")
		return
	}
	view, presets, err := readBookSelection(db, owner)
	if err != nil {
		log.Printf("book %s: re-read: %v", what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error reading the Book's selection")
		return
	}
	resp := gin.H{"selection": view, "presets": presets}
	for k, v := range extra {
		resp[k] = v
	}
	c.JSON(status, resp)
}

func bindBody(c *gin.Context, allowed ...string) (map[string]json.RawMessage, bool) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return nil, false
	}
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range body {
		if !ok[k] {
			respondError(c, http.StatusBadRequest, "unknown_field", fmt.Sprintf("unknown field %q", k))
			return nil, false
		}
	}
	return body, true
}

// PutInvoicingBookSelectionHandler serves PUT /invoicing/book/selection with
// {income, expense_all, expense, preset}. preset names what the ticks came
// from ("work_only", one of her presets' ids, or empty); it is optional.
func PutInvoicingBookSelectionHandler(c *gin.Context) {
	body, ok := bindBody(c, "income", "expense_all", "expense", "preset")
	if !ok {
		return
	}
	next, refusal := bookCountingBody(body)
	if refusal != nil {
		respondError(c, refusal.status, refusal.code, refusal.message)
		return
	}
	var preset string
	if raw, ok := body["preset"]; ok && json.Unmarshal(raw, &preset) != nil {
		respondError(c, http.StatusBadRequest, "invalid_field", "preset must be a string")
		return
	}
	if r := workOnlyRule(next, false); r != nil {
		respondError(c, r.status, r.code, r.message)
		return
	}
	bookWrite(c, "selection", http.StatusOK, func(tx *gorm.DB, owner uint) (*rowRefusal, gin.H, error) {
		if r, err := checkBookCounting(tx, owner, next); r != nil || err != nil {
			return r, nil, err
		}
		if _, r, err := presetForSelection(tx, owner, preset); r != nil || err != nil {
			return r, nil, err
		}
		s, err := data_models.GetInvoicingBookSelection(tx, owner)
		if err != nil {
			return nil, nil, err
		}
		before := gin.H{"counting": data_models.BookWorkOnly().Normalised(), "preset": data_models.BookPresetWorkOnly}
		if s == nil {
			s = &data_models.InvoicingBookSelection{OwnerUserID: owner}
		} else {
			before = gin.H{"counting": s.Counting(), "preset": s.Preset}
		}
		s.SetCounting(next)
		s.Preset = preset
		if err := tx.Save(s).Error; err != nil {
			return nil, nil, err
		}
		details := auditDetails(gin.H{"before": before, "after": gin.H{"counting": next.Normalised(), "preset": preset}})
		return nil, nil, data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionUpdate, invoicingEntityBookSelection, s.ID, details)
	})
}

// presetName validates a preset's name: one line, 1-60 characters, not the
// built-in's, and not one she already uses (case-insensitively).
func presetName(tx *gorm.DB, owner uint, raw json.RawMessage, exceptID uint) (string, *rowRefusal, error) {
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return "", refuseRow(http.StatusBadRequest, "invalid_field", "name must be a string"), nil
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxBookPresetNameRunes || strings.Contains(name, "\n") || hasControlChars(name) || hasFormatChars(name) {
		return "", refuseRow(http.StatusBadRequest, "invalid_field", "name must be one line of 1-%d characters", maxBookPresetNameRunes), nil
	}
	if strings.EqualFold(name, data_models.BookPresetWorkOnlyName) {
		return "", refuseRow(http.StatusConflict, "duplicate_preset", "%q is the built-in preset's name", data_models.BookPresetWorkOnlyName), nil
	}
	own, err := data_models.ListInvoicingBookPresets(tx, owner)
	if err != nil {
		return "", nil, err
	}
	for _, p := range own {
		if p.ID != exceptID && strings.EqualFold(p.Name, name) {
			return "", refuseRow(http.StatusConflict, "duplicate_preset", "a preset named %q already exists", p.Name), nil
		}
	}
	return name, nil, nil
}

// CreateInvoicingBookPresetHandler serves POST /invoicing/book/presets with
// {name, income, expense_all, expense}.
func CreateInvoicingBookPresetHandler(c *gin.Context) {
	body, ok := bindBody(c, "name", "income", "expense_all", "expense")
	if !ok {
		return
	}
	if _, ok := body["name"]; !ok {
		respondError(c, http.StatusBadRequest, "invalid_field", "name is required")
		return
	}
	counting, refusal := bookCountingBody(body)
	if refusal != nil {
		respondError(c, refusal.status, refusal.code, refusal.message)
		return
	}
	if r := workOnlyRule(counting, true); r != nil {
		respondError(c, r.status, r.code, r.message)
		return
	}
	bookWrite(c, "preset create", http.StatusCreated, func(tx *gorm.DB, owner uint) (*rowRefusal, gin.H, error) {
		name, r, err := presetName(tx, owner, body["name"], 0)
		if r != nil || err != nil {
			return r, nil, err
		}
		if r, err := checkBookCounting(tx, owner, counting); r != nil || err != nil {
			return r, nil, err
		}
		p := data_models.InvoicingBookPreset{OwnerUserID: owner, Name: name}
		p.SetCounting(counting)
		if err := tx.Create(&p).Error; err != nil {
			return nil, nil, err
		}
		details := auditDetails(gin.H{"after": gin.H{"name": name, "counting": p.Counting()}})
		if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionCreate, invoicingEntityBookPreset, p.ID, details); err != nil {
			return nil, nil, err
		}
		return nil, gin.H{"preset": presetView(p)}, nil
	})
}

// bookPresetID reads :id. The built-in is refused here, since it can be
// neither updated nor deleted.
func bookPresetID(c *gin.Context) (uint, bool) {
	raw := c.Param("id")
	if raw == data_models.BookPresetWorkOnly {
		respondError(c, http.StatusConflict, "builtin_preset", "Work only is built in and cannot be changed or deleted")
		return 0, false
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		RespondWithError(c, http.StatusBadRequest, "Invalid preset id")
		return 0, false
	}
	return uint(n), true
}

// UpdateInvoicingBookPresetHandler serves PUT /invoicing/book/presets/:id
// with {income, expense_all, expense} and an optional new name.
func UpdateInvoicingBookPresetHandler(c *gin.Context) {
	id, ok := bookPresetID(c)
	if !ok {
		return
	}
	body, ok := bindBody(c, "name", "income", "expense_all", "expense")
	if !ok {
		return
	}
	counting, refusal := bookCountingBody(body)
	if refusal != nil {
		respondError(c, refusal.status, refusal.code, refusal.message)
		return
	}
	if r := workOnlyRule(counting, true); r != nil {
		respondError(c, r.status, r.code, r.message)
		return
	}
	bookWrite(c, "preset update", http.StatusOK, func(tx *gorm.DB, owner uint) (*rowRefusal, gin.H, error) {
		p, err := data_models.GetInvoicingBookPreset(tx, owner, id)
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			return refuseRow(http.StatusNotFound, "not_found", "preset %d not found", id), nil, nil
		}
		before := gin.H{"name": p.Name, "counting": p.Counting()}
		if raw, ok := body["name"]; ok {
			name, r, err := presetName(tx, owner, raw, p.ID)
			if r != nil || err != nil {
				return r, nil, err
			}
			p.Name = name
		}
		if r, err := checkBookCounting(tx, owner, counting); r != nil || err != nil {
			return r, nil, err
		}
		p.SetCounting(counting)
		if err := tx.Save(p).Error; err != nil {
			return nil, nil, err
		}
		details := auditDetails(gin.H{"before": before, "after": gin.H{"name": p.Name, "counting": p.Counting()}})
		if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionUpdate, invoicingEntityBookPreset, p.ID, details); err != nil {
			return nil, nil, err
		}
		return nil, gin.H{"preset": presetView(*p)}, nil
	})
}

// DeleteInvoicingBookPresetHandler serves DELETE /invoicing/book/presets/:id.
// If the current selection came from it, the selection keeps its ticks and
// names no preset.
func DeleteInvoicingBookPresetHandler(c *gin.Context) {
	id, ok := bookPresetID(c)
	if !ok {
		return
	}
	bookWrite(c, "preset delete", http.StatusOK, func(tx *gorm.DB, owner uint) (*rowRefusal, gin.H, error) {
		p, err := data_models.GetInvoicingBookPreset(tx, owner, id)
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			return refuseRow(http.StatusNotFound, "not_found", "preset %d not found", id), nil, nil
		}
		if err := tx.Delete(p).Error; err != nil {
			return nil, nil, err
		}
		key := strconv.FormatUint(uint64(p.ID), 10)
		if err := tx.Model(&data_models.InvoicingBookSelection{}).Where("owner_user_id = ? AND preset = ?", owner, key).
			Update("preset", "").Error; err != nil {
			return nil, nil, err
		}
		details := auditDetails(gin.H{"before": gin.H{"name": p.Name, "counting": p.Counting()}})
		if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionDelete, invoicingEntityBookPreset, p.ID, details); err != nil {
			return nil, nil, err
		}
		return nil, gin.H{"deleted": key}, nil
	})
}
