package data_models

import (
	"github.com/jinzhu/gorm"
)

type Role struct {
	gorm.Model
	RoleName        string `json:"role_name" gorm:"unique;not null"`
	RoleDescription string `json:"role_description"`
}

// SeedDimRoles seeds the database with default roles
func SeedRoles(db *gorm.DB) error {
	roles := []Role{
		{RoleName: "Admin", RoleDescription: "Administrator with full access"},
		{RoleName: "User", RoleDescription: "Regular user with limited access"},
		{RoleName: "Guest", RoleDescription: "Guest user with minimal access"},
		{RoleName: "Gamer", RoleDescription: "User with access to gaming features"},
		{RoleName: "Deleted", RoleDescription: "User with no access"},
		{RoleName: "Trusted", RoleDescription: "User with trusted access"},
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
