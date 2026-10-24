package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Card types and templates (system_3 #8634, subtask #8643). Admin-only routes
// under /cardgen/types and /cardgen/templates. Design of record: #8634 c53338
// sections 1 to 3; the spec is validated by data_models.ParseTemplateSpec.

var cardgenSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// CardTypeResponse is the DTO for one card type.
type CardTypeResponse struct {
	ID              uint      `json:"id"`
	Slug            string    `json:"slug"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	FrontTemplateID *uint     `json:"front_template_id"`
	BackTemplateID  *uint     `json:"back_template_id"`
	DisplayOrder    int       `json:"display_order"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func cardTypeResponse(t *data_models.CardType) CardTypeResponse {
	return CardTypeResponse{ID: t.ID, Slug: t.Slug, Name: t.Name, Description: t.Description, FrontTemplateID: t.FrontTemplateID, BackTemplateID: t.BackTemplateID, DisplayOrder: t.DisplayOrder, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

// CardTemplateResponse is the DTO for one template; Spec is the current
// version's body and is included on the single-template routes.
type CardTemplateResponse struct {
	ID               uint            `json:"id"`
	Slug             string          `json:"slug"`
	Name             string          `json:"name"`
	ParentTemplateID *uint           `json:"parent_template_id"`
	ParentSlug       string          `json:"parent_slug"`
	CurrentVersionNo int             `json:"current_version_no"`
	Spec             json.RawMessage `json:"spec,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// CardTemplateVersionResponse is the DTO for one version.
type CardTemplateVersionResponse struct {
	VersionNo       int             `json:"version_no"`
	Note            string          `json:"note"`
	CreatedByUserID uint            `json:"created_by_user_id"`
	CreatedAt       time.Time       `json:"created_at"`
	Spec            json.RawMessage `json:"spec"`
}

func cardTemplateVersionResponse(v *data_models.CardTemplateVersion) CardTemplateVersionResponse {
	return CardTemplateVersionResponse{VersionNo: v.VersionNo, Note: v.Note, CreatedByUserID: v.CreatedByUserID, CreatedAt: v.CreatedAt, Spec: v.Spec}
}

func cardTemplateResponse(db *gorm.DB, t *data_models.CardTemplate, withSpec bool) (CardTemplateResponse, error) {
	out := CardTemplateResponse{ID: t.ID, Slug: t.Slug, Name: t.Name, ParentTemplateID: t.ParentTemplateID, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	if t.ParentTemplateID != nil {
		var p data_models.CardTemplate
		if err := db.Unscoped().Select("slug").Where("id = ?", *t.ParentTemplateID).First(&p).Error; err == nil {
			out.ParentSlug = p.Slug
		}
	}
	if t.CurrentVersionID != nil {
		v, err := data_models.CurrentTemplateVersion(db, t)
		if err != nil {
			return out, err
		}
		out.CurrentVersionNo = v.VersionNo
		if withSpec {
			out.Spec = v.Spec
		}
	}
	return out, nil
}

func cardgenIDParam(c *gin.Context, name, what string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, what+" id must be a positive integer")
		return 0, false
	}
	return uint(id), true
}

func cardTemplateByID(c *gin.Context, db *gorm.DB) (*data_models.CardTemplate, bool) {
	id, ok := cardgenIDParam(c, "id", "Template")
	if !ok {
		return nil, false
	}
	var t data_models.CardTemplate
	if err := db.Where("id = ?", id).First(&t).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			RespondWithError(c, http.StatusNotFound, "Template not found")
		} else {
			log.Printf("Cardgen: load template %d failed: %v", id, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to load template")
		}
		return nil, false
	}
	return &t, true
}

// specOrBadRequest validates a spec body and answers 400 with the element and
// prop named on refusal.
func specOrBadRequest(c *gin.Context, db *gorm.DB, raw json.RawMessage) bool {
	if len(raw) == 0 {
		RespondWithError(c, http.StatusBadRequest, "spec is required")
		return false
	}
	if _, err := data_models.ParseTemplateSpec(raw, db); err != nil {
		var se *data_models.SpecError
		if errors.As(err, &se) {
			RespondWithError(c, http.StatusBadRequest, se.Error())
		} else {
			log.Printf("Cardgen: spec validation failed: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to validate spec")
		}
		return false
	}
	return true
}

func templateExists(db *gorm.DB, id uint) (bool, error) {
	var n int
	err := db.Model(&data_models.CardTemplate{}).Where("id = ?", id).Count(&n).Error
	return n > 0, err
}

// ---- types -----------------------------------------------------------------

// ListCardTypesHandler handles GET /cardgen/types.
func ListCardTypesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var rows []data_models.CardType
	if err := db.Order("display_order ASC, id ASC").Find(&rows).Error; err != nil {
		log.Printf("Cardgen: list types failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list card types")
		return
	}
	out := make([]CardTypeResponse, 0, len(rows))
	for i := range rows {
		out = append(out, cardTypeResponse(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{"types": out})
}

type cardTypeBody struct {
	Slug            *string `json:"slug"`
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	FrontTemplateID *uint   `json:"front_template_id"`
	BackTemplateID  *uint   `json:"back_template_id"`
	DisplayOrder    *int    `json:"display_order"`
}

func (b *cardTypeBody) templatesExist(db *gorm.DB) (string, error) {
	for _, ref := range []struct {
		name string
		id   *uint
	}{{"front_template_id", b.FrontTemplateID}, {"back_template_id", b.BackTemplateID}} {
		if ref.id == nil {
			continue
		}
		ok, err := templateExists(db, *ref.id)
		if err != nil {
			return "", err
		}
		if !ok {
			return ref.name + " does not name a template", nil
		}
	}
	return "", nil
}

// CreateCardTypeHandler handles POST /cardgen/types.
func CreateCardTypeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var b cardTypeBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON with slug and name")
		return
	}
	if b.Slug == nil || !cardgenSlugPattern.MatchString(*b.Slug) {
		RespondWithError(c, http.StatusBadRequest, "slug is required: lowercase letters, digits and underscores, starting with a letter")
		return
	}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" || len(*b.Name) > 60 {
		RespondWithError(c, http.StatusBadRequest, "name is required (at most 60 characters)")
		return
	}
	if msg, err := b.templatesExist(db); err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to check templates")
		return
	} else if msg != "" {
		RespondWithError(c, http.StatusBadRequest, msg)
		return
	}
	var n int
	db.Model(&data_models.CardType{}).Where("slug = ?", *b.Slug).Count(&n)
	if n > 0 {
		RespondWithError(c, http.StatusConflict, "A card type with that slug already exists")
		return
	}
	t := data_models.CardType{Slug: *b.Slug, Name: strings.TrimSpace(*b.Name), FrontTemplateID: b.FrontTemplateID, BackTemplateID: b.BackTemplateID}
	if b.Description != nil {
		t.Description = *b.Description
	}
	if b.DisplayOrder != nil {
		t.DisplayOrder = *b.DisplayOrder
	}
	if err := db.Create(&t).Error; err != nil {
		log.Printf("Cardgen: create type failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create card type")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"type": cardTypeResponse(&t)})
}

// UpdateCardTypeHandler handles PUT /cardgen/types/:id; every field is
// optional, the slug cannot change.
func UpdateCardTypeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id, ok := cardgenIDParam(c, "id", "Card type")
	if !ok {
		return
	}
	var t data_models.CardType
	if err := db.Where("id = ?", id).First(&t).Error; err != nil {
		RespondWithError(c, http.StatusNotFound, "Card type not found")
		return
	}
	var b cardTypeBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON")
		return
	}
	if b.Slug != nil && *b.Slug != t.Slug {
		RespondWithError(c, http.StatusBadRequest, "slug cannot change")
		return
	}
	if msg, err := b.templatesExist(db); err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to check templates")
		return
	} else if msg != "" {
		RespondWithError(c, http.StatusBadRequest, msg)
		return
	}
	updates := map[string]interface{}{}
	if b.Name != nil {
		if strings.TrimSpace(*b.Name) == "" || len(*b.Name) > 60 {
			RespondWithError(c, http.StatusBadRequest, "name must be 1 to 60 characters")
			return
		}
		updates["name"] = strings.TrimSpace(*b.Name)
	}
	if b.Description != nil {
		updates["description"] = *b.Description
	}
	if b.FrontTemplateID != nil {
		updates["front_template_id"] = *b.FrontTemplateID
	}
	if b.BackTemplateID != nil {
		updates["back_template_id"] = *b.BackTemplateID
	}
	if b.DisplayOrder != nil {
		updates["display_order"] = *b.DisplayOrder
	}
	if len(updates) > 0 {
		if err := db.Model(&t).Updates(updates).Error; err != nil {
			log.Printf("Cardgen: update type %d failed: %v", id, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to update card type")
			return
		}
	}
	db.Where("id = ?", id).First(&t)
	c.JSON(http.StatusOK, gin.H{"type": cardTypeResponse(&t)})
}

// ---- templates -------------------------------------------------------------

// ListCardTemplatesHandler handles GET /cardgen/templates.
func ListCardTemplatesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var rows []data_models.CardTemplate
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		log.Printf("Cardgen: list templates failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list templates")
		return
	}
	out := make([]CardTemplateResponse, 0, len(rows))
	for i := range rows {
		r, err := cardTemplateResponse(db, &rows[i], false)
		if err != nil {
			log.Printf("Cardgen: template %d response failed: %v", rows[i].ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to list templates")
			return
		}
		out = append(out, r)
	}
	c.JSON(http.StatusOK, gin.H{"templates": out})
}

type cardTemplateBody struct {
	Slug             *string         `json:"slug"`
	Name             *string         `json:"name"`
	ParentTemplateID *uint           `json:"parent_template_id"`
	ClearParent      bool            `json:"clear_parent"`
	Spec             json.RawMessage `json:"spec"`
	Note             string          `json:"note"`
}

// CreateCardTemplateHandler handles POST /cardgen/templates: the template and
// its version 1 in one call.
func CreateCardTemplateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	var b cardTemplateBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON with slug, name and spec")
		return
	}
	if b.Slug == nil || !cardgenSlugPattern.MatchString(*b.Slug) {
		RespondWithError(c, http.StatusBadRequest, "slug is required: lowercase letters, digits and underscores, starting with a letter")
		return
	}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" || len(*b.Name) > 60 {
		RespondWithError(c, http.StatusBadRequest, "name is required (at most 60 characters)")
		return
	}
	if len(b.Note) > 200 {
		RespondWithError(c, http.StatusBadRequest, "note is at most 200 characters")
		return
	}
	if !specOrBadRequest(c, db, b.Spec) {
		return
	}
	if b.ParentTemplateID != nil {
		exists, err := templateExists(db, *b.ParentTemplateID)
		if err != nil {
			RespondWithError(c, http.StatusInternalServerError, "Failed to check the parent template")
			return
		}
		if !exists {
			RespondWithError(c, http.StatusBadRequest, "parent_template_id does not name a template")
			return
		}
		if _, err := data_models.TemplateChain(db, *b.ParentTemplateID); err != nil {
			RespondWithError(c, http.StatusBadRequest, "parent chain is not usable: "+err.Error())
			return
		}
	}
	var n int
	db.Unscoped().Model(&data_models.CardTemplate{}).Where("slug = ?", *b.Slug).Count(&n)
	if n > 0 {
		RespondWithError(c, http.StatusConflict, "A template with that slug already exists")
		return
	}
	t := data_models.CardTemplate{Slug: *b.Slug, Name: strings.TrimSpace(*b.Name), ParentTemplateID: b.ParentTemplateID}
	if err := db.Create(&t).Error; err != nil {
		log.Printf("Cardgen: create template failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create template")
		return
	}
	note := b.Note
	if note == "" {
		note = "created"
	}
	if _, err := data_models.AddTemplateVersion(db, &t, b.Spec, note, userID); err != nil {
		log.Printf("Cardgen: create template v1 failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create template")
		return
	}
	r, err := cardTemplateResponse(db, &t, true)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load template")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"template": r})
}

// GetCardTemplateHandler handles GET /cardgen/templates/:id.
func GetCardTemplateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	r, err := cardTemplateResponse(db, t, true)
	if err != nil {
		log.Printf("Cardgen: template %d response failed: %v", t.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to load template")
		return
	}
	c.JSON(http.StatusOK, gin.H{"template": r})
}

// UpdateCardTemplateHandler handles PUT /cardgen/templates/:id: name and
// parent (set, or clear with clear_parent). Re-parenting is refused when it
// would make a cycle. The spec changes through versions, not here.
func UpdateCardTemplateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	var b cardTemplateBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON")
		return
	}
	if b.Slug != nil && *b.Slug != t.Slug {
		RespondWithError(c, http.StatusBadRequest, "slug cannot change")
		return
	}
	if len(b.Spec) > 0 {
		RespondWithError(c, http.StatusBadRequest, "spec changes go through POST /cardgen/templates/:id/versions")
		return
	}
	updates := map[string]interface{}{}
	if b.Name != nil {
		if strings.TrimSpace(*b.Name) == "" || len(*b.Name) > 60 {
			RespondWithError(c, http.StatusBadRequest, "name must be 1 to 60 characters")
			return
		}
		updates["name"] = strings.TrimSpace(*b.Name)
	}
	switch {
	case b.ClearParent:
		updates["parent_template_id"] = nil
	case b.ParentTemplateID != nil:
		exists, err := templateExists(db, *b.ParentTemplateID)
		if err != nil {
			RespondWithError(c, http.StatusInternalServerError, "Failed to check the parent template")
			return
		}
		if !exists {
			RespondWithError(c, http.StatusBadRequest, "parent_template_id does not name a template")
			return
		}
		cyc, err := data_models.WouldCycle(db, t.ID, b.ParentTemplateID)
		if err != nil {
			RespondWithError(c, http.StatusBadRequest, "parent chain is not usable: "+err.Error())
			return
		}
		if cyc {
			RespondWithError(c, http.StatusBadRequest, "that parent would make the template extend itself")
			return
		}
		updates["parent_template_id"] = *b.ParentTemplateID
	}
	if len(updates) > 0 {
		if err := db.Model(t).Updates(updates).Error; err != nil {
			log.Printf("Cardgen: update template %d failed: %v", t.ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to update template")
			return
		}
	}
	db.Where("id = ?", t.ID).First(t)
	r, err := cardTemplateResponse(db, t, true)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load template")
		return
	}
	c.JSON(http.StatusOK, gin.H{"template": r})
}

// DeleteCardTemplateHandler handles DELETE /cardgen/templates/:id (soft);
// refused while a type or a child template references it.
func DeleteCardTemplateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	referenced, err := data_models.TemplateIsReferenced(db, t.ID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to check references")
		return
	}
	if referenced {
		RespondWithError(c, http.StatusConflict, "Template is used by a card type or extended by another template")
		return
	}
	if err := db.Delete(t).Error; err != nil {
		log.Printf("Cardgen: delete template %d failed: %v", t.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete template")
		return
	}
	RespondWithSuccess(c, gin.H{"id": t.ID}, "Template deleted")
}

// ListCardTemplateVersionsHandler handles GET /cardgen/templates/:id/versions.
func ListCardTemplateVersionsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	versions, err := data_models.TemplateVersions(db, t.ID)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to list versions")
		return
	}
	out := make([]CardTemplateVersionResponse, 0, len(versions))
	for i := range versions {
		out = append(out, cardTemplateVersionResponse(&versions[i]))
	}
	current := 0
	if t.CurrentVersionID != nil {
		if v, err := data_models.CurrentTemplateVersion(db, t); err == nil {
			current = v.VersionNo
		}
	}
	c.JSON(http.StatusOK, gin.H{"versions": out, "current_version_no": current})
}

// CreateCardTemplateVersionHandler handles POST /cardgen/templates/:id/versions.
func CreateCardTemplateVersionHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	var b cardTemplateBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON with spec")
		return
	}
	if len(b.Note) > 200 {
		RespondWithError(c, http.StatusBadRequest, "note is at most 200 characters")
		return
	}
	if !specOrBadRequest(c, db, b.Spec) {
		return
	}
	v, err := data_models.AddTemplateVersion(db, t, b.Spec, b.Note, userID)
	if err != nil {
		log.Printf("Cardgen: add version to template %d failed: %v", t.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to save version")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"version": cardTemplateVersionResponse(v)})
}

// RestoreCardTemplateVersionHandler handles POST /cardgen/templates/:id/versions/:n/restore.
func RestoreCardTemplateVersionHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n < 1 {
		RespondWithError(c, http.StatusBadRequest, "Version number must be a positive integer")
		return
	}
	v, err := data_models.RestoreTemplateVersion(db, t, n, userID)
	if err == data_models.ErrTemplateVersionNotFound {
		RespondWithError(c, http.StatusNotFound, "Version not found")
		return
	}
	if err != nil {
		log.Printf("Cardgen: restore v%d of template %d failed: %v", n, t.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to restore version")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"version": cardTemplateVersionResponse(v)})
}

// ResolveCardTemplateHandler handles GET /cardgen/templates/:id/resolved.
func ResolveCardTemplateHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	t, ok := cardTemplateByID(c, db)
	if !ok {
		return
	}
	res, err := data_models.ResolveTemplate(db, t.ID)
	switch {
	case err == nil:
	case errors.Is(err, data_models.ErrTemplateCycle), errors.Is(err, data_models.ErrTemplateChainTooDeep), errors.Is(err, data_models.ErrTemplateMissing):
		RespondWithError(c, http.StatusConflict, "Template chain cannot be resolved: "+err.Error())
		return
	default:
		log.Printf("Cardgen: resolve template %d failed: %v", t.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to resolve template")
		return
	}
	r, err := cardTemplateResponse(db, t, false)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load template")
		return
	}
	c.JSON(http.StatusOK, gin.H{"template": r, "resolved": res})
}
