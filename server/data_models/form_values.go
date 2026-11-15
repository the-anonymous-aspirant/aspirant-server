package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

// UserFormValues is one remembered form per user per tool (system_3 #11190,
// design_conventions §3.154). Values holds JSON that the server serialised
// from a validated, fixed set of keys (handlers/form_values.go); a request
// body is never stored as sent, so no arbitrary JSON can land here.
//
// Scoped to the session's user only, like Scratchpad. At rest it has the same
// protection and backups as the rest of the member data in this database, and
// no field encryption of its own (§3.154).
type UserFormValues struct {
	UserID    uint   `gorm:"primary_key;auto_increment:false"`
	Tool      string `gorm:"primary_key;type:varchar(64)"`
	Values    string `gorm:"type:text;not null"`
	UpdatedAt time.Time
	CreatedAt time.Time
}

func (UserFormValues) TableName() string { return "user_form_values" }

// GetFormValues returns the user's record for tool, or (nil, nil) when none is
// stored: an empty form is not an error.
func GetFormValues(db *gorm.DB, userID uint, tool string) (*UserFormValues, error) {
	var fv UserFormValues
	err := db.Where("user_id = ? AND tool = ?", userID, tool).First(&fv).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fv, nil
}

// UpsertFormValues stores values (already validated and serialised by the
// caller) as the user's record for tool. GORM v1 has no ON CONFLICT clause,
// so this reads, then creates or saves, as UpsertScratchpad does.
func UpsertFormValues(db *gorm.DB, userID uint, tool, values string) (*UserFormValues, error) {
	existing, err := GetFormValues(db, userID, tool)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing == nil {
		fv := UserFormValues{UserID: userID, Tool: tool, Values: values, UpdatedAt: now, CreatedAt: now}
		if err := db.Create(&fv).Error; err != nil {
			return nil, err
		}
		return &fv, nil
	}
	existing.Values = values
	existing.UpdatedAt = now
	if err := db.Save(existing).Error; err != nil {
		return nil, err
	}
	return existing, nil
}

// DeleteFormValues removes the user's record for tool. Delete means delete: the
// row goes, it is not emptied. Removing a record that is not there is not an
// error, so a repeated DELETE stays idempotent.
func DeleteFormValues(db *gorm.DB, userID uint, tool string) error {
	return db.Where("user_id = ? AND tool = ?", userID, tool).Delete(&UserFormValues{}).Error
}

// DeleteAllFormValuesForUser removes every remembered form of the user. It is
// called from User.DeleteUser: deleting a user is a soft delete (gorm.Model),
// so a foreign-key cascade would never fire and the values would outlive the
// account (system_3 #11190).
func DeleteAllFormValuesForUser(db *gorm.DB, userID uint) error {
	return db.Where("user_id = ?", userID).Delete(&UserFormValues{}).Error
}
