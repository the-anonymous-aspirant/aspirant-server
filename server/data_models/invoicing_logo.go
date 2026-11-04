package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// invoicingLogoID is the fixed primary key of the single issuer-logo row.
const invoicingLogoID = 1

// InvoicingProfileLogo is the issuer's logo, printed at the top of every
// invoice issued while it is set (system_3 #9858). One row, kept apart from
// invoicing_profiles so the image bytes never enter an invoice's
// profile_snapshot or a profile audit row. Content is the normalized PNG the
// upload produced, never the uploaded file itself. An issued invoice keeps the
// logo it was printed with inside its stored PDF, so replacing or removing
// the logo changes no issued document.
type InvoicingProfileLogo struct {
	ID        uint      `json:"-" gorm:"primary_key"`
	Content   []byte    `json:"-" gorm:"not null"`
	SHA256    string    `json:"sha256" gorm:"column:sha256;type:text;not null"`
	Width     int       `json:"width" gorm:"not null"`
	Height    int       `json:"height" gorm:"not null"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy uint      `json:"updated_by"`
}

func (InvoicingProfileLogo) TableName() string { return "invoicing_profile_logos" }

// GetInvoicingProfileLogo returns the logo, or nil when none is set.
func GetInvoicingProfileLogo(db *gorm.DB) (*InvoicingProfileLogo, error) {
	var l InvoicingProfileLogo
	err := db.Where("id = ?", invoicingLogoID).First(&l).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// SaveInvoicingProfileLogo upserts the single logo row.
func SaveInvoicingProfileLogo(db *gorm.DB, l *InvoicingProfileLogo, userID uint) error {
	l.ID = invoicingLogoID
	l.UpdatedAt = time.Now().UTC()
	l.UpdatedBy = userID
	return db.Save(l).Error
}

// DeleteInvoicingProfileLogo removes the logo. It reports whether there was
// one to remove.
func DeleteInvoicingProfileLogo(db *gorm.DB) (bool, error) {
	res := db.Where("id = ?", invoicingLogoID).Delete(&InvoicingProfileLogo{})
	return res.RowsAffected > 0, res.Error
}
