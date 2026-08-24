package data_models

import (
	"fmt"
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
	// AvatarETag is the MD5 (content ETag) of the user's current profile
	// picture in asset storage, or "" when no avatar is set. It is never
	// serialised directly — the browser-facing avatar URL is derived from it
	// via AvatarURLFor and carried on the response DTOs. Added for #4170.
	AvatarETag string `json:"-"`
}

// AvatarURLFor builds the browser-facing URL the SPA binds to an <img> for a
// user's avatar, or "" when the user has no avatar. The URL points at the
// authenticated per-user avatar-serve route (any logged-in caller may fetch it,
// unlike the public /fetch-object gate which only Trusted/Admin bypass), and
// carries the content ETag as a ?v= cache-buster so a replaced avatar is
// re-fetched rather than served stale from the browser cache. The /api prefix
// is the nginx-proxied client path (the server itself is mounted without it).
func AvatarURLFor(id uint, etag string) string {
	if etag == "" {
		return ""
	}
	return fmt.Sprintf("/api/data_models/users/%d/avatar?v=%s", id, etag)
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
	// AvatarURL mirrors PublicUserResponse's field so the message-board author
	// strip renders avatars for an ADMIN caller too. GetAllUsersHandler returns
	// this DTO (not the public one) to admins, so without this the admin — the
	// only caller who ever gets UserResponse from the user list — sees the
	// shared placeholder for every author, including their own drawn icon
	// (#4223 item 2, operator ask #1544). The avatar is public identity, not
	// PII (#4170), so adding it to the richer admin DTO widens nothing.
	AvatarURL string `json:"avatar_url"`
	// DisplayName is the user's current public display name (#4223 item 4). The
	// message-board author strip prefers it over the raw username. It is not set
	// by ToResponse (which has no DB access) — the handler resolves it via
	// CurrentDisplayName(s) and stamps it on the DTO; it falls back to Username
	// when unresolved. Public identity, not PII.
	DisplayName string `json:"display_name"`
}

// PublicUserResponse is the DTO surfaced to non-Admin callers of the
// user-list / user-by-id routes. It strips email and comment so a
// lowest-priv authenticated caller cannot harvest PII across the
// user table (CWE-639 mitigation — security-finding #1380), and it
// omits access_role so a non-Admin cannot enumerate which account is
// the Admin (security-finding #3093 — CWE-639/A01). The only non-Admin
// consumer is the message board, which reads ID + username + avatar_url +
// display_name (all public identity, not PII — what the message-board author
// strip renders in place of the shared placeholder, #4170/#4223).
type PublicUserResponse struct {
	ID       uint   `json:"ID"`
	Username string `json:"username"`
	// DisplayName is stamped by the handler via CurrentDisplayName(s) (ToPublic-
	// Response has no DB access); falls back to Username when unresolved (#4223
	// item 4). Preferred over Username by the board's author strip.
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
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
		AvatarURL:  AvatarURLFor(u.ID, u.AvatarETag),
	}
}

// ToPublicResponse converts a User (with preloaded Role) to the
// PII-stripped DTO used for non-Admin callers.
func (u *User) ToPublicResponse() PublicUserResponse {
	return PublicUserResponse{
		ID:        u.ID,
		Username:  u.Username,
		AvatarURL: AvatarURLFor(u.ID, u.AvatarETag),
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

// AfterCreate opens the user's initial display-name row (display_name =
// username) so every creation path gets a public display identity without
// per-caller edits (security-finding #3094). It never fails the user create:
// display-name bookkeeping is best-effort, and the AutoMigrate backfill
// (BackfillDisplayNames) reconciles any user that slips through. The HasTable
// guard keeps unit tests that create users without migrating the display-name
// table (or a boot before the table exists) working unchanged.
func (u *User) AfterCreate(tx *gorm.DB) error {
	if !tx.HasTable(&UserDisplayName{}) {
		return nil
	}
	var count int
	tx.Model(&UserDisplayName{}).Where("user_id = ?", u.ID).Count(&count)
	if count == 0 {
		tx.Create(&UserDisplayName{
			UserID:      u.ID,
			DisplayName: u.Username,
			ValidFrom:   time.Now(),
		})
	}
	return nil
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
