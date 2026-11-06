package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// InvoicingProfileLogo is the issuer's logo, printed at the top of every
// invoice issued while it is set (system_3 #9858). One row per owner (system_3
// #10054), kept apart from
// invoicing_profiles so the image bytes never enter an invoice's
// profile_snapshot or a profile audit row. Content is the normalized PNG the
// upload produced, never the uploaded file itself. An issued invoice keeps the
// logo it was printed with inside its stored PDF, so replacing or removing
// the logo changes no issued document.
type InvoicingProfileLogo struct {
	ID          uint      `json:"-" gorm:"primary_key"`
	OwnerUserID uint      `json:"-" gorm:"column:owner_user_id;not null;default:0;unique_index:idx_invoicing_profile_logos_owner"`
	Content     []byte    `json:"-" gorm:"not null"`
	SHA256      string    `json:"sha256" gorm:"column:sha256;type:text;not null"`
	Width       int       `json:"width" gorm:"not null"`
	Height      int       `json:"height" gorm:"not null"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   uint      `json:"updated_by"`
}

func (InvoicingProfileLogo) TableName() string { return "invoicing_profile_logos" }

// GetInvoicingProfileLogo returns the owner's logo, or nil when none is set.
func GetInvoicingProfileLogo(db *gorm.DB, owner uint) (*InvoicingProfileLogo, error) {
	var l InvoicingProfileLogo
	err := db.Where("owner_user_id = ?", owner).First(&l).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// SaveInvoicingProfileLogo upserts the owner's logo row. The signed-in user is
// both the owner and the editor.
func SaveInvoicingProfileLogo(db *gorm.DB, l *InvoicingProfileLogo, userID uint) error {
	existing, err := GetInvoicingProfileLogo(db, userID)
	if err != nil {
		return err
	}
	l.ID = 0
	if existing != nil {
		l.ID = existing.ID
	}
	l.OwnerUserID = userID
	l.UpdatedAt = time.Now().UTC()
	l.UpdatedBy = userID
	return db.Save(l).Error
}

// DeleteInvoicingProfileLogo removes the owner's logo. It reports whether
// there was one to remove.
func DeleteInvoicingProfileLogo(db *gorm.DB, owner uint) (bool, error) {
	res := db.Where("owner_user_id = ?", owner).Delete(&InvoicingProfileLogo{})
	return res.RowsAffected > 0, res.Error
}
