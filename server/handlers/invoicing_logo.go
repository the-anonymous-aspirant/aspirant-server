package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"

	"aspirant-online/server/data_models"
	"aspirant-online/server/invoicing_pdf"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// The issuer's logo (system_3 #9858), printed top right on every invoice
// issued while it is set. The upload is normalized before it is stored
// (invoicing_pdf.NormalizeLogo): only the re-encoded PNG is kept, never the
// file as uploaded. Behind the same owner+Admin gate as the profile.

const (
	invoicingEntityProfileLogo = "profile_logo"
	invoicingActionUpload      = "upload"
)

// PutInvoicingLogoHandler replaces the logo with the multipart field `logo`.
func PutInvoicingLogoHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	file, err := c.FormFile("logo")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "No logo file received")
		return
	}
	if file.Size > invoicing_pdf.MaxLogoBytes {
		RespondWithError(c, http.StatusBadRequest, fmt.Sprintf("the logo is larger than %d MB", invoicing_pdf.MaxLogoBytes>>20))
		return
	}
	f, err := file.Open()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read the logo")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, invoicing_pdf.MaxLogoBytes+1))
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to read the logo")
		return
	}

	logo, err := invoicing_pdf.NormalizeLogo(raw)
	if invoicing_pdf.IsLogoError(err) {
		RespondWithError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		log.Printf("Error normalizing invoicing logo: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to process the logo")
		return
	}
	sum := sha256.Sum256(logo.PNG)
	stored := data_models.InvoicingProfileLogo{
		Content: logo.PNG,
		SHA256:  hex.EncodeToString(sum[:]),
		Width:   logo.Width,
		Height:  logo.Height,
	}
	uploadSum := sha256.Sum256(raw)

	userID := invoicingUserID(c)
	tx := db.Begin()
	before, err := data_models.GetInvoicingProfileLogo(tx)
	if err != nil {
		respondInvoicingInternal(c, tx, "saving the logo", err)
		return
	}
	if err := data_models.SaveInvoicingProfileLogo(tx, &stored, userID); err != nil {
		respondInvoicingInternal(c, tx, "saving the logo", err)
		return
	}
	details := gin.H{
		"after":         gin.H{"sha256": stored.SHA256, "width": stored.Width, "height": stored.Height, "bytes": len(stored.Content)},
		"upload_sha256": hex.EncodeToString(uploadSum[:]),
		"upload_bytes":  len(raw),
	}
	if before != nil {
		details["before"] = gin.H{"sha256": before.SHA256, "width": before.Width, "height": before.Height, "bytes": len(before.Content)}
	}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionUpload, invoicingEntityProfileLogo, stored.ID, auditDetails(details)); err != nil {
		respondInvoicingInternal(c, tx, "saving the logo", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "saving the logo", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"logo": stored, "saved": true})
}

// GetInvoicingLogoHandler serves the stored logo PNG.
func GetInvoicingLogoHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	logo, err := data_models.GetInvoicingProfileLogo(db)
	if err != nil {
		respondInvoicingInternal(c, nil, "loading the logo", err)
		return
	}
	if logo == nil {
		RespondWithError(c, http.StatusNotFound, "No logo is set")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Content-SHA256", logo.SHA256)
	c.Data(http.StatusOK, "image/png", logo.Content)
}

// DeleteInvoicingLogoHandler removes the logo. Invoices issued with it keep it.
func DeleteInvoicingLogoHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	userID := invoicingUserID(c)
	tx := db.Begin()
	before, err := data_models.GetInvoicingProfileLogo(tx)
	if err != nil {
		respondInvoicingInternal(c, tx, "removing the logo", err)
		return
	}
	if before == nil {
		tx.Rollback()
		RespondWithError(c, http.StatusNotFound, "No logo is set")
		return
	}
	if _, err := data_models.DeleteInvoicingProfileLogo(tx); err != nil {
		respondInvoicingInternal(c, tx, "removing the logo", err)
		return
	}
	details := gin.H{"before": gin.H{"sha256": before.SHA256, "width": before.Width, "height": before.Height, "bytes": len(before.Content)}}
	if err := data_models.RecordInvoicingAuditEvent(tx, userID, invoicingActionDelete, invoicingEntityProfileLogo, before.ID, auditDetails(details)); err != nil {
		respondInvoicingInternal(c, tx, "removing the logo", err)
		return
	}
	if err := tx.Commit().Error; err != nil {
		respondInvoicingInternal(c, nil, "removing the logo", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// invoiceLogo loads the logo for an invoice being issued, ready to print.
func invoiceLogo(tx *gorm.DB) (*invoicing_pdf.Logo, string, error) {
	stored, err := data_models.GetInvoicingProfileLogo(tx)
	if err != nil || stored == nil {
		return nil, "", err
	}
	return &invoicing_pdf.Logo{PNG: stored.Content, Width: stored.Width, Height: stored.Height}, stored.SHA256, nil
}
