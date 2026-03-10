package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	gorm.Model
	Username string `json:"username" gorm:"unique;not null"`
	Email    string `json:"email" gorm:"unique;not null"`
	Password string `json:"password,omitempty"`
	RoleID   uint   `json:"-"`
	Role     Role   `json:"-" gorm:"foreignkey:RoleID;save_associations:false"`
	Comment  string `json:"comment"`
}

// UserResponse is the DTO used for API responses, exposing role as a string.
type UserResponse struct {
	ID         uint      `json:"ID"`
	CreatedAt  time.Time `json:"CreatedAt"`
	UpdatedAt  time.Time `json:"UpdatedAt"`
	Username   string    `json:"username"`
	Email      string    `json:"email"`
	AccessRole string    `json:"access_role"`
	Comment    string    `json:"comment"`
}

// ToResponse converts a User (with preloaded Role) to the API response DTO.
func (u *User) ToResponse() UserResponse {
	return UserResponse{
		ID:         u.ID,
		CreatedAt:  u.CreatedAt,
		UpdatedAt:  u.UpdatedAt,
		Username:   u.Username,
		Email:      u.Email,
		AccessRole: u.Role.RoleName,
		Comment:    u.Comment,
	}
}

// GetAllUsers retrieves all users from the database with their roles preloaded.
func GetAllUsers(db *gorm.DB) ([]User, error) {
	var users []User
	err := db.Preload("Role").Find(&users).Error
	if err != nil {
		return nil, err
	}
	return users, nil
}

// HashPassword hashes the user's password
func (u *User) HashPassword(password string) error {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Password = string(hashedPassword)
	return nil
}

// CheckPassword checks if the provided password is correct
func (u *User) CheckPassword(password string) error {
	return bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password))
}

// CreateUser creates a new user
func (u *User) CreateUser(db *gorm.DB) error {
	return db.Create(u).Error
}

// UpdateUser updates the user's information
func (u *User) UpdateUser(db *gorm.DB) error {
	return db.Save(u).Error
}

// DeleteUser deletes a user from the database
func (u *User) DeleteUser(db *gorm.DB) error {
	return db.Delete(u).Error
}

func GetUserById(db *gorm.DB, id string) (*User, error) {
	var user User
	err := db.Preload("Role").Where("id = ?", id).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}
