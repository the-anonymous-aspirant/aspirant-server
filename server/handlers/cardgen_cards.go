package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Cards (system_3 #8634, subtask #8646). Admin-only routes under
// /cardgen/cards. Values are validated by data_models.ParseCardData; faces are
// resolved by data_models.ResolveCard.

// CardResponse is the DTO for one card; Data is the current version's body
// and is included on the single-card routes.
type CardResponse struct {
	ID                 uint            `json:"id"`
	Slug               string          `json:"slug"`
	Name               string          `json:"name"`
	CardTypeID         uint            `json:"card_type_id"`
	TemplateID         *uint           `json:"template_id"`
	GoalCardID         *uint           `json:"goal_card_id"`
	RelationshipTypeID *uint           `json:"relationship_type_id"`
	DisplayOrder       int             `json:"display_order"`
	CurrentVersionNo   int             `json:"current_version_no"`
	Data               json.RawMessage `json:"data,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// CardVersionResponse is the DTO for one card version.
type CardVersionResponse struct {
	VersionNo       int             `json:"version_no"`
	Note            string          `json:"note"`
	CreatedByUserID uint            `json:"created_by_user_id"`
	// CreatedByName is the author's current display name ("system" for the
	// seeds, "" when the user no longer exists); system_3 #8758.
	CreatedByName string          `json:"created_by_name"`
	CreatedAt     time.Time       `json:"created_at"`
	Data          json.RawMessage `json:"data"`
}

func cardVersionResponse(v *data_models.CardVersion, createdByName string) CardVersionResponse {
	return CardVersionResponse{VersionNo: v.VersionNo, Note: v.Note, CreatedByUserID: v.CreatedByUserID, CreatedByName: createdByName, CreatedAt: v.CreatedAt, Data: v.Data}
}

func cardResponse(db *gorm.DB, c *data_models.Card, withData bool) (CardResponse, error) {
	out := CardResponse{ID: c.ID, Slug: c.Slug, Name: c.Name, CardTypeID: c.CardTypeID, TemplateID: c.TemplateID, GoalCardID: c.GoalCardID, RelationshipTypeID: c.RelationshipTypeID, DisplayOrder: c.DisplayOrder, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.CurrentVersionID != nil {
		v, err := data_models.CurrentCardVersion(db, c)
		if err != nil {
			return out, err
		}
		out.CurrentVersionNo = v.VersionNo
		if withData {
			out.Data = v.Data
		}
	}
	return out, nil
}

func cardByID(c *gin.Context, db *gorm.DB) (*data_models.Card, bool) {
	id, ok := cardgenIDParam(c, "id", "Card")
	if !ok {
		return nil, false
	}
	var card data_models.Card
	if err := db.Where("id = ?", id).First(&card).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			RespondWithError(c, http.StatusNotFound, "Card not found")
		} else {
			log.Printf("Cardgen: load card %d failed: %v", id, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to load card")
		}
		return nil, false
	}
	return &card, true
}

func cardDataOrBadRequest(c *gin.Context, db *gorm.DB, raw json.RawMessage) bool {
	if len(raw) == 0 {
		RespondWithError(c, http.StatusBadRequest, "data is required")
		return false
	}
	if _, err := data_models.ParseCardData(raw, db); err != nil {
		var se *data_models.SpecError
		if errors.As(err, &se) {
			RespondWithError(c, http.StatusBadRequest, se.Error())
		} else {
			RespondWithError(c, http.StatusInternalServerError, "Failed to validate data")
		}
		return false
	}
	return true
}

func cardTypeExists(db *gorm.DB, id uint) (bool, error) {
	var n int
	err := db.Model(&data_models.CardType{}).Where("id = ?", id).Count(&n).Error
	return n > 0, err
}

type cardBody struct {
	Slug          *string         `json:"slug"`
	Name          *string         `json:"name"`
	CardTypeID    *uint           `json:"card_type_id"`
	TemplateID    *uint           `json:"template_id"`
	ClearTemplate bool            `json:"clear_template"`
	DisplayOrder  *int            `json:"display_order"`
	Data          json.RawMessage `json:"data"`
	Note          string          `json:"note"`
}

// ListCardsHandler handles GET /cardgen/cards (?type_id=).
func ListCardsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	q := db.Order("card_type_id ASC, display_order ASC, id ASC")
	if t := strings.TrimSpace(c.Query("type_id")); t != "" {
		id, err := strconv.ParseUint(t, 10, 64)
		if err != nil || id == 0 {
			RespondWithError(c, http.StatusBadRequest, "type_id must be a positive integer")
			return
		}
		q = q.Where("card_type_id = ?", uint(id))
	}
	var rows []data_models.Card
	if err := q.Find(&rows).Error; err != nil {
		log.Printf("Cardgen: list cards failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list cards")
		return
	}
	out := make([]CardResponse, 0, len(rows))
	for i := range rows {
		r, err := cardResponse(db, &rows[i], false)
		if err != nil {
			log.Printf("Cardgen: card %d response failed: %v", rows[i].ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to list cards")
			return
		}
		out = append(out, r)
	}
	c.JSON(http.StatusOK, gin.H{"cards": out})
}

// CreateCardHandler handles POST /cardgen/cards: the card and its version 1.
func CreateCardHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	var b cardBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON with slug, name, card_type_id and data")
		return
	}
	if b.Slug == nil || !cardgenSlugPattern.MatchString(*b.Slug) {
		RespondWithError(c, http.StatusBadRequest, "slug is required: lowercase letters, digits and underscores, starting with a letter")
		return
	}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" || len(*b.Name) > 80 {
		RespondWithError(c, http.StatusBadRequest, "name is required (at most 80 characters)")
		return
	}
	if b.CardTypeID == nil {
		RespondWithError(c, http.StatusBadRequest, "card_type_id is required")
		return
	}
	if exists, err := cardTypeExists(db, *b.CardTypeID); err != nil || !exists {
		RespondWithError(c, http.StatusBadRequest, "card_type_id does not name a card type")
		return
	}
	if b.TemplateID != nil {
		if exists, err := templateExists(db, *b.TemplateID); err != nil || !exists {
			RespondWithError(c, http.StatusBadRequest, "template_id does not name a template")
			return
		}
	}
	if len(b.Note) > 200 {
		RespondWithError(c, http.StatusBadRequest, "note is at most 200 characters")
		return
	}
	if len(b.Data) == 0 {
		b.Data = json.RawMessage(`{"values": {}}`)
	}
	if !cardDataOrBadRequest(c, db, b.Data) {
		return
	}
	var n int
	db.Unscoped().Model(&data_models.Card{}).Where("slug = ?", *b.Slug).Count(&n)
	if n > 0 {
		RespondWithError(c, http.StatusConflict, "A card with that slug already exists")
		return
	}
	card := data_models.Card{CardTypeID: *b.CardTypeID, TemplateID: b.TemplateID, Slug: *b.Slug, Name: strings.TrimSpace(*b.Name)}
	if b.DisplayOrder != nil {
		card.DisplayOrder = *b.DisplayOrder
	}
	if err := db.Create(&card).Error; err != nil {
		log.Printf("Cardgen: create card failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create card")
		return
	}
	note := b.Note
	if note == "" {
		note = "created"
	}
	if _, err := data_models.AddCardVersion(db, &card, b.Data, note, userID); err != nil {
		log.Printf("Cardgen: create card v1 failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create card")
		return
	}
	r, err := cardResponse(db, &card, true)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load card")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"card": r})
}

// GetCardHandler handles GET /cardgen/cards/:id.
func GetCardHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	r, err := cardResponse(db, card, true)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load card")
		return
	}
	c.JSON(http.StatusOK, gin.H{"card": r})
}

// UpdateCardHandler handles PUT /cardgen/cards/:id: name, type, template
// override (or clear_template) and display order. Values change through
// versions.
func UpdateCardHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	var b cardBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON")
		return
	}
	if b.Slug != nil && *b.Slug != card.Slug {
		RespondWithError(c, http.StatusBadRequest, "slug cannot change")
		return
	}
	if len(b.Data) > 0 {
		RespondWithError(c, http.StatusBadRequest, "data changes go through POST /cardgen/cards/:id/versions")
		return
	}
	updates := map[string]interface{}{}
	if b.Name != nil {
		if strings.TrimSpace(*b.Name) == "" || len(*b.Name) > 80 {
			RespondWithError(c, http.StatusBadRequest, "name must be 1 to 80 characters")
			return
		}
		updates["name"] = strings.TrimSpace(*b.Name)
	}
	if b.CardTypeID != nil {
		if exists, err := cardTypeExists(db, *b.CardTypeID); err != nil || !exists {
			RespondWithError(c, http.StatusBadRequest, "card_type_id does not name a card type")
			return
		}
		updates["card_type_id"] = *b.CardTypeID
	}
	switch {
	case b.ClearTemplate:
		updates["template_id"] = nil
	case b.TemplateID != nil:
		if exists, err := templateExists(db, *b.TemplateID); err != nil || !exists {
			RespondWithError(c, http.StatusBadRequest, "template_id does not name a template")
			return
		}
		updates["template_id"] = *b.TemplateID
	}
	if b.DisplayOrder != nil {
		updates["display_order"] = *b.DisplayOrder
	}
	if len(updates) > 0 {
		if err := db.Model(card).Updates(updates).Error; err != nil {
			log.Printf("Cardgen: update card %d failed: %v", card.ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to update card")
			return
		}
	}
	db.Where("id = ?", card.ID).First(card)
	r, err := cardResponse(db, card, true)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load card")
		return
	}
	c.JSON(http.StatusOK, gin.H{"card": r})
}

// DeleteCardHandler handles DELETE /cardgen/cards/:id (soft).
func DeleteCardHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	if err := db.Delete(card).Error; err != nil {
		log.Printf("Cardgen: delete card %d failed: %v", card.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete card")
		return
	}
	RespondWithSuccess(c, gin.H{"id": card.ID}, "Card deleted")
}

// ListCardVersionsHandler handles GET /cardgen/cards/:id/versions.
func ListCardVersionsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	versions, err := data_models.CardVersions(db, card.ID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to list versions")
		return
	}
	ids := make([]uint, 0, len(versions))
	for i := range versions {
		ids = append(ids, versions[i].CreatedByUserID)
	}
	names := cardgenActorNames(db, ids)
	out := make([]CardVersionResponse, 0, len(versions))
	for i := range versions {
		out = append(out, cardVersionResponse(&versions[i], names[versions[i].CreatedByUserID]))
	}
	current := 0
	if v, err := data_models.CurrentCardVersion(db, card); err == nil {
		current = v.VersionNo
	}
	c.JSON(http.StatusOK, gin.H{"versions": out, "current_version_no": current})
}

// CreateCardVersionHandler handles POST /cardgen/cards/:id/versions.
func CreateCardVersionHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	var b cardBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON with data")
		return
	}
	if len(b.Note) > 200 {
		RespondWithError(c, http.StatusBadRequest, "note is at most 200 characters")
		return
	}
	if !cardDataOrBadRequest(c, db, b.Data) {
		return
	}
	v, err := data_models.AddCardVersion(db, card, b.Data, b.Note, userID)
	if err != nil {
		log.Printf("Cardgen: add version to card %d failed: %v", card.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to save version")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"version": cardVersionResponse(v, cardgenActorName(db, v.CreatedByUserID))})
}

// RestoreCardVersionHandler handles POST /cardgen/cards/:id/versions/:n/restore.
func RestoreCardVersionHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n < 1 {
		RespondWithError(c, http.StatusBadRequest, "Version number must be a positive integer")
		return
	}
	v, err := data_models.RestoreCardVersion(db, card, n, userID)
	if err == data_models.ErrCardVersionNotFound {
		RespondWithError(c, http.StatusNotFound, "Version not found")
		return
	}
	if err != nil {
		log.Printf("Cardgen: restore v%d of card %d failed: %v", n, card.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to restore version")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"version": cardVersionResponse(v, cardgenActorName(db, v.CreatedByUserID))})
}

// ResolveCardHandler handles GET /cardgen/cards/:id/resolved.
func ResolveCardHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	card, ok := cardByID(c, db)
	if !ok {
		return
	}
	res, err := data_models.ResolveCard(db, card)
	switch {
	case err == nil:
	case errors.Is(err, data_models.ErrCardHasNoFront), errors.Is(err, data_models.ErrTemplateCycle), errors.Is(err, data_models.ErrTemplateChainTooDeep), errors.Is(err, data_models.ErrTemplateMissing):
		RespondWithError(c, http.StatusConflict, "Card cannot be resolved: "+err.Error())
		return
	default:
		log.Printf("Cardgen: resolve card %d failed: %v", card.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to resolve card")
		return
	}
	r, err := cardResponse(db, card, false)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load card")
		return
	}
	c.JSON(http.StatusOK, gin.H{"card": r, "resolved": res})
}
