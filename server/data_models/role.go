package data_models

import (
	"github.com/jinzhu/gorm"
)

type Role struct {
	gorm.Model
	RoleName        string `json:"role_name" gorm:"unique;not null"`
	RoleDescription string `json:"role_description"`
}

// SeedRoles seeds the four access tiers (system_3 epic #5113, subtask A2).
// The vocabulary migrated from the legacy six-role set (Admin, User, Guest,
// Gamer, Deleted, Trusted) to four tiers: public is the ABSENCE of a role
// (unauthenticated), so the role table holds only the three authenticated
// tiers plus Blocked. Admin keeps its name (admin sessions/gates unaffected);
// Trusted→Member, {User,Guest,Gamer}→Viewer, Deleted→Blocked. The user-FK
// remap and legacy-row cleanup run idempotently in server.AutoMigrate.
func SeedRoles(db *gorm.DB) error {
	roles := []Role{
		{RoleName: "Admin", RoleDescription: "Full access to everything"},
		{RoleName: "Member", RoleDescription: "Viewer access plus the member area (file storage and such)"},
		{RoleName: "Viewer", RoleDescription: "Public access plus the applications"},
		{RoleName: "Blocked", RoleDescription: "Authenticated but with no access"},
	}

	for _, role := range roles {
		if err := db.FirstOrCreate(&role, Role{RoleName: role.RoleName}).Error; err != nil {
			return err
		}
	}
	return nil
}

// GetRoleByName retrieves a role by its name
func GetRoleByName(db *gorm.DB, name string) (*Role, error) {
	var role Role
	if err := db.Where("role_name = ?", name).First(&role).Error; err != nil {
		return nil, err
	}
	return &role, nil
}

// GetAllRoles retrieves all roles from the database
func GetAllRoles(db *gorm.DB) ([]Role, error) {
	var roles []Role
	err := db.Find(&roles).Error
	if err != nil {
		return nil, err
	}
	return roles, nil
}
