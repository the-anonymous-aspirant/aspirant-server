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

// Export profiles (system_3 #8634, subtask #8644). Admin-only CRUD under
// /cardgen/export-profiles; the spec is validated by
// data_models.ParseExportProfileSpec and stored as sent.

// CardExportProfileResponse is the DTO for one profile.
type CardExportProfileResponse struct {
	ID        uint                          `json:"id"`
	Name      string                        `json:"name"`
	Spec      data_models.ExportProfileSpec `json:"spec"`
	CreatedAt time.Time                     `json:"created_at"`
	UpdatedAt time.Time                     `json:"updated_at"`
}

func cardExportProfileResponse(p *data_models.CardExportProfile) (CardExportProfileResponse, error) {
	spec, err := data_models.ParseExportProfileSpec(p.Spec)
	if err != nil {
		return CardExportProfileResponse{}, err
	}
	return CardExportProfileResponse{ID: p.ID, Name: p.Name, Spec: *spec, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}, nil
}

type cardExportProfileBody struct {
	Name *string         `json:"name"`
	Spec json.RawMessage `json:"spec"`
}

func exportProfileByID(c *gin.Context, db *gorm.DB) (*data_models.CardExportProfile, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Profile id must be a positive integer")
		return nil, false
	}
	var p data_models.CardExportProfile
	if err := db.Where("id = ?", uint(id)).First(&p).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			RespondWithError(c, http.StatusNotFound, "Export profile not found")
		} else {
			log.Printf("Cardgen: load export profile %d failed: %v", id, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to load export profile")
		}
		return nil, false
	}
	return &p, true
}

func exportSpecOrBadRequest(c *gin.Context, raw json.RawMessage) bool {
	if len(raw) == 0 {
		RespondWithError(c, http.StatusBadRequest, "spec is required")
		return false
	}
	if _, err := data_models.ParseExportProfileSpec(raw); err != nil {
		var se *data_models.ExportSpecError
		if errors.As(err, &se) {
			RespondWithError(c, http.StatusBadRequest, se.Error())
		} else {
			RespondWithError(c, http.StatusInternalServerError, "Failed to validate spec")
		}
		return false
	}
	return true
}

func exportNameOK(name string) bool {
	n := strings.TrimSpace(name)
	return n != "" && len(n) <= 60
}

// ListCardExportProfilesHandler handles GET /cardgen/export-profiles.
func ListCardExportProfilesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var rows []data_models.CardExportProfile
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		log.Printf("Cardgen: list export profiles failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list export profiles")
		return
	}
	out := make([]CardExportProfileResponse, 0, len(rows))
	for i := range rows {
		r, err := cardExportProfileResponse(&rows[i])
		if err != nil {
			log.Printf("Cardgen: export profile %d is unreadable: %v", rows[i].ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to list export profiles")
			return
		}
		out = append(out, r)
	}
	c.JSON(http.StatusOK, gin.H{"profiles": out})
}

// CreateCardExportProfileHandler handles POST /cardgen/export-profiles.
func CreateCardExportProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	var b cardExportProfileBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON with name and spec")
		return
	}
	if b.Name == nil || !exportNameOK(*b.Name) {
		RespondWithError(c, http.StatusBadRequest, "name is required (at most 60 characters)")
		return
	}
	if !exportSpecOrBadRequest(c, b.Spec) {
		return
	}
	name := strings.TrimSpace(*b.Name)
	var n int
	db.Unscoped().Model(&data_models.CardExportProfile{}).Where("name = ?", name).Count(&n)
	if n > 0 {
		RespondWithError(c, http.StatusConflict, "An export profile with that name already exists")
		return
	}
	p := data_models.CardExportProfile{Name: name, Spec: b.Spec}
	if err := db.Create(&p).Error; err != nil {
		log.Printf("Cardgen: create export profile failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create export profile")
		return
	}
	r, _ := cardExportProfileResponse(&p)
	c.JSON(http.StatusCreated, gin.H{"profile": r})
}

// GetCardExportProfileHandler handles GET /cardgen/export-profiles/:id.
func GetCardExportProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	p, ok := exportProfileByID(c, db)
	if !ok {
		return
	}
	r, err := cardExportProfileResponse(p)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Export profile is unreadable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": r})
}

// UpdateCardExportProfileHandler handles PUT /cardgen/export-profiles/:id;
// name and spec are each optional, a given spec replaces the whole spec.
func UpdateCardExportProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	p, ok := exportProfileByID(c, db)
	if !ok {
		return
	}
	var b cardExportProfileBody
	if err := c.ShouldBindJSON(&b); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Body must be JSON")
		return
	}
	updates := map[string]interface{}{}
	if b.Name != nil {
		if !exportNameOK(*b.Name) {
			RespondWithError(c, http.StatusBadRequest, "name must be 1 to 60 characters")
			return
		}
		name := strings.TrimSpace(*b.Name)
		if name != p.Name {
			var n int
			db.Unscoped().Model(&data_models.CardExportProfile{}).Where("name = ? AND id <> ?", name, p.ID).Count(&n)
			if n > 0 {
				RespondWithError(c, http.StatusConflict, "An export profile with that name already exists")
				return
			}
			updates["name"] = name
		}
	}
	if len(b.Spec) > 0 {
		if !exportSpecOrBadRequest(c, b.Spec) {
			return
		}
		updates["spec"] = b.Spec
	}
	if len(updates) > 0 {
		if err := db.Model(p).Updates(updates).Error; err != nil {
			log.Printf("Cardgen: update export profile %d failed: %v", p.ID, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to update export profile")
			return
		}
	}
	db.Where("id = ?", p.ID).First(p)
	r, err := cardExportProfileResponse(p)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Export profile is unreadable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": r})
}

// DeleteCardExportProfileHandler handles DELETE /cardgen/export-profiles/:id (soft).
func DeleteCardExportProfileHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	p, ok := exportProfileByID(c, db)
	if !ok {
		return
	}
	if err := db.Delete(p).Error; err != nil {
		log.Printf("Cardgen: delete export profile %d failed: %v", p.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete export profile")
		return
	}
	RespondWithSuccess(c, gin.H{"id": p.ID}, "Export profile deleted")
}
