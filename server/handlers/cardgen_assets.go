package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Card editor asset store (system_3 #8634, subtask #8642). Admin-only routes
// under /cardgen/assets: upload, list, get, serve content, soft delete. The
// store is content-addressed (sha256) on the server's LocalStorage; the design
// of record is #8634 c53338 section 4.

// cardAssetFormField is the multipart field the uploader posts the file in.
const cardAssetFormField = "file"

// cardAssetFormSlack is the multipart framing allowed around one file at the
// cap (boundaries, part headers, a long filename) before the body reader
// refuses the request.
const cardAssetFormSlack = 64 << 10

// CardAssetResponse is the DTO for one asset. ContentURL is the browser-facing
// path (the /api prefix is nginx's; the server itself is mounted without it).
type CardAssetResponse struct {
	ID               uint      `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	SHA256           string    `json:"sha256"`
	OriginalFilename string    `json:"original_filename"`
	MediaType        string    `json:"media_type"`
	Kind             string    `json:"kind"`
	ByteSize         int64     `json:"byte_size"`
	PixelWidth       int       `json:"pixel_width"`
	PixelHeight      int       `json:"pixel_height"`
	UploadedByUserID uint      `json:"uploaded_by_user_id"`
	ContentURL       string    `json:"content_url"`
}

func cardAssetResponse(a *data_models.CardAsset) CardAssetResponse {
	return CardAssetResponse{
		ID:               a.ID,
		CreatedAt:        a.CreatedAt,
		SHA256:           a.SHA256,
		OriginalFilename: a.OriginalFilename,
		MediaType:        a.MediaType,
		Kind:             a.Kind,
		ByteSize:         a.ByteSize,
		PixelWidth:       a.PixelWidth,
		PixelHeight:      a.PixelHeight,
		UploadedByUserID: a.UploadedByUserID,
		ContentURL:       fmt.Sprintf("/api/cardgen/assets/%d/content", a.ID),
	}
}

// cardAssetStore fetches the LocalStorage main.go put in the context, or
// answers 500 and reports false.
func cardAssetStore(c *gin.Context) (*storage.LocalStorage, bool) {
	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return nil, false
	}
	ls, ok := store.(*storage.LocalStorage)
	if !ok || ls == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return nil, false
	}
	return ls, true
}

// cardAssetByID loads a live (not soft-deleted) asset from the :id param, or
// answers 404 / 400 and reports false.
func cardAssetByID(c *gin.Context, db *gorm.DB) (*data_models.CardAsset, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		RespondWithError(c, http.StatusBadRequest, "Asset id must be a positive integer")
		return nil, false
	}
	var a data_models.CardAsset
	if err := db.Where("id = ?", uint(id)).First(&a).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			RespondWithError(c, http.StatusNotFound, "Asset not found")
		} else {
			log.Printf("Cardgen: load asset %d failed: %v", id, err)
			RespondWithError(c, http.StatusInternalServerError, "Failed to load asset")
		}
		return nil, false
	}
	return &a, true
}

// safeAssetFilename reduces the client's filename to a base name a
// Content-Disposition header can carry; the store never uses it as a path.
func safeAssetFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" {
		return "asset"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// UploadCardAssetHandler handles POST /cardgen/assets — store one file
// (multipart field "file") in the card asset store.
//
// The size is checked twice (the multipart header, then a LimitReader on the
// bytes), the type is sniffed from the bytes and must be on the allowlist, and
// identity is the sha256 of the content: an upload whose bytes already exist
// answers 200 with the existing row and "deduplicated": true (a soft-deleted
// twin is restored rather than duplicated); a new file answers 201. Nothing in
// the store is ever overwritten.
func UploadCardAssetHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID, ok := callerUserID(c)
	if !ok {
		RespondWithError(c, http.StatusUnauthorized, "Authentication required")
		return
	}

	// Bound the request before the multipart parser sees it: Gin spools a form
	// past 32 MiB to temp files before any handler check runs, so the cap must
	// sit on the body itself (security review, #8634 c53354 item 3). The slack
	// is for the multipart framing around one file at the cap.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, data_models.MaxCardAssetBytes+cardAssetFormSlack)

	file, err := c.FormFile(cardAssetFormField)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("File exceeds the %d byte limit", data_models.MaxCardAssetBytes))
			return
		}
		RespondWithError(c, http.StatusBadRequest, "No file received (multipart field \"file\")")
		return
	}
	if file.Size > data_models.MaxCardAssetBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("File exceeds the %d byte limit", data_models.MaxCardAssetBytes))
		return
	}

	f, err := file.Open()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read file")
		return
	}
	defer f.Close()

	content, err := io.ReadAll(io.LimitReader(f, data_models.MaxCardAssetBytes+1))
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read file")
		return
	}
	if len(content) > data_models.MaxCardAssetBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("File exceeds the %d byte limit", data_models.MaxCardAssetBytes))
		return
	}
	if len(content) == 0 {
		RespondWithError(c, http.StatusBadRequest, "File is empty")
		return
	}

	head := content
	if len(head) > 512 {
		head = head[:512]
	}
	mediaType, kind, ok := data_models.DetectCardAssetType(head)
	if !ok {
		RespondWithError(c, http.StatusBadRequest, "Unsupported file type (allowed: PNG, JPEG, WebP, GIF, SVG, PDF, TTF, OTF, WOFF, WOFF2)")
		return
	}
	if kind == data_models.CardAssetKindSVG {
		if why := data_models.ValidateSVGContent(content); why != "" {
			RespondWithError(c, http.StatusBadRequest, "Refused: "+why)
			return
		}
	}
	w, h := 0, 0
	if kind == data_models.CardAssetKindImage {
		w, h = data_models.CardAssetPixelSize(content)
		if w > data_models.MaxCardAssetPixels || h > data_models.MaxCardAssetPixels {
			RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("Image is %dx%d px; the limit is %d px per side", w, h, data_models.MaxCardAssetPixels))
			return
		}
	}

	sum := sha256.Sum256(content)
	shaHex := hex.EncodeToString(sum[:])

	// Identity check, soft-deleted rows included: the same bytes are one asset.
	var existing data_models.CardAsset
	err = db.Unscoped().Where("sha256 = ?", shaHex).First(&existing).Error
	switch {
	case err == nil:
		if existing.DeletedAt != nil {
			if err := db.Unscoped().Model(&existing).Update("deleted_at", nil).Error; err != nil {
				log.Printf("Cardgen: restore asset %d failed: %v", existing.ID, err)
				RespondWithError(c, http.StatusInternalServerError, "Failed to store asset")
				return
			}
			existing.DeletedAt = nil
		}
		c.JSON(http.StatusOK, gin.H{"asset": cardAssetResponse(&existing), "deduplicated": true})
		return
	case err != gorm.ErrRecordNotFound:
		log.Printf("Cardgen: lookup asset by sha256 failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to store asset")
		return
	}

	assets, ok := cardAssetStore(c)
	if !ok {
		return
	}
	key := data_models.CardAssetStorageKey(shaHex)
	if err := assets.Put(key, bytes.NewReader(content)); err != nil {
		log.Printf("Cardgen: store asset %s failed: %v", key, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to store asset")
		return
	}

	a := data_models.CardAsset{
		SHA256:           shaHex,
		StorageKey:       key,
		OriginalFilename: safeAssetFilename(file.Filename),
		MediaType:        mediaType,
		Kind:             kind,
		ByteSize:         int64(len(content)),
		PixelWidth:       w,
		PixelHeight:      h,
		UploadedByUserID: userID,
	}
	if err := db.Create(&a).Error; err != nil {
		log.Printf("Cardgen: record asset %s failed: %v", shaHex, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to store asset")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"asset": cardAssetResponse(&a), "deduplicated": false})
}

// ListCardAssetsHandler handles GET /cardgen/assets — every live asset, newest
// first, optionally filtered by ?kind=image|svg|pdf|font.
func ListCardAssetsHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	q := db.Order("id DESC")
	if kind := strings.TrimSpace(c.Query("kind")); kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var rows []data_models.CardAsset
	if err := q.Find(&rows).Error; err != nil {
		log.Printf("Cardgen: list assets failed: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list assets")
		return
	}
	out := make([]CardAssetResponse, 0, len(rows))
	for i := range rows {
		out = append(out, cardAssetResponse(&rows[i]))
	}
	c.JSON(http.StatusOK, gin.H{"assets": out})
}

// GetCardAssetHandler handles GET /cardgen/assets/:id — one asset's metadata.
func GetCardAssetHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	a, ok := cardAssetByID(c, db)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"asset": cardAssetResponse(a)})
}

// GetCardAssetContentHandler handles GET /cardgen/assets/:id/content — the
// bytes. The type served is the one sniffed at upload, with nosniff so the
// browser cannot second-guess it; only raster images are served inline, every
// other kind (SVG, PDF, fonts) is an attachment. The URL maps to immutable
// content, so a private day-long cache is safe.
func GetCardAssetContentHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	a, ok := cardAssetByID(c, db)
	if !ok {
		return
	}
	assets, ok := cardAssetStore(c)
	if !ok {
		return
	}
	data, err := assets.Get(a.StorageKey)
	if err != nil {
		log.Printf("Cardgen: bytes for asset %d (%s) missing: %v", a.ID, a.StorageKey, err)
		RespondWithError(c, http.StatusNotFound, "Asset content not found")
		return
	}

	disposition := "attachment"
	if data_models.CardAssetServesInline(a.MediaType) {
		disposition = "inline"
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.OriginalFilename}))
	c.Header("Cache-Control", "private, max-age=86400")
	c.Data(http.StatusOK, a.MediaType, data)
}

// DeleteCardAssetHandler handles DELETE /cardgen/assets/:id — soft-delete the
// row. The bytes stay: the store is content-addressed and an upload of the
// same content restores the row. (#8647 adds the refusal while a template or
// card still references the asset.)
func DeleteCardAssetHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	a, ok := cardAssetByID(c, db)
	if !ok {
		return
	}
	if err := db.Delete(a).Error; err != nil {
		log.Printf("Cardgen: delete asset %d failed: %v", a.ID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete asset")
		return
	}
	RespondWithSuccess(c, gin.H{"id": a.ID}, "Asset deleted")
}
