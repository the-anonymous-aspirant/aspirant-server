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
	// SessionEpoch is bumped to revoke every session issued before now.
	//
	// A COUNTER, not a timestamp, and that is the whole point. The first three
	// attempts at this compared the token's issue time against a revocation
	// time (system_3 #5275) and each got a boundary wrong, because comparing
	// two independently-taken clock readings to establish an ordering is racy
	// at every resolution — I lost that argument at second scale twice and at
	// millisecond scale once. The database orders the increment against the
	// read for free, so there is no window to get wrong.
	//
	// A token carries the epoch its user had when it was minted; it is current
	// only while those still match. Zero for every account that predates this,
	// and zero is what a token with no epoch claim is read as, so the migration
	// needs no backfill and no existing session is disturbed until something
	// actually revokes.
	//
	// `sessions_valid_from` from the first attempt is left in the database as a
	// dead column: AutoMigrate does not drop columns, and removing it is not
	// worth a hand-written migration on a table this important.
	SessionEpoch uint `json:"-" gorm:"not null;default:0"`

	// EmailVerifiedAt is the moment the address was confirmed	// EmailVerifiedAt is the moment the address was confirmed, and nil until it
	// is. A nullable timestamp rather than a bool: "when" answers questions a
	// bool cannot, and NULL is the honest value for every account created
	// before self-service sign-up existed. Those are backfilled in AutoMigrate
	// — see the note there, because reading NULL as "unverified" without the
	// backfill locks out every existing user.
	EmailVerifiedAt *time.Time `json:"-"`
	Role            Role       `json:"-" gorm:"foreignkey:RoleID;save_associations:false"`
	Comment         string     `json:"comment"`
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
	// EmailVerifiedAt is when the account confirmed its address, or null when
	// it never has (#5290). It is the column the operator moderates on: a
	// self-service account that never followed its link is what a bot sign-up
	// looks like from the admin page, and before this the roster could not tell
	// one from a real user.
	//
	// Admin DTO only. PublicUserResponse deliberately does not gain it — that
	// DTO exists to keep account facts off the non-admin path (#1380/#3093),
	// and whether a stranger has confirmed their address is exactly such a
	// fact. A nullable timestamp rather than a bool for the same reason the
	// column is one: "when" answers questions "whether" cannot.
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
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
		ID:              u.ID,
		CreatedAt:       u.CreatedAt,
		UpdatedAt:       u.UpdatedAt,
		Username:        u.Username,
		Email:           u.Email,
		AccessRole:      u.Role.RoleName,
		Comment:         u.Comment,
		AvatarURL:       AvatarURLFor(u.ID, u.AvatarETag),
		EmailVerifiedAt: u.EmailVerifiedAt,
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

// IsEmailVerified reports whether the account's address has been confirmed.
//
// An unverified account exists but cannot authenticate (see LoginHandler). The
// method exists so no caller has to remember that nil is the unverified state.
func (u *User) IsEmailVerified() bool {
	return u.EmailVerifiedAt != nil
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

// MarkEmailVerifiedNow stamps the address as confirmed, as of this instant.
//
// For the two paths where an administrator creates the account rather than a
// person signing themselves up: POST /bootstrap/admin and the Admin-only
// POST /data_models/users.
//
// Those accounts are verified by the admin's act. Self-service verification
// exists to prove that whoever typed an address can receive mail at it; when an
// admin enters it there is no such claim to test, and bootstrap additionally
// runs only on an empty database, where there is nobody to send anything to.
//
// It is a shared method rather than two inline literals so the reason lives in
// one place and a third creation path cannot quietly acquire the behaviour
// without acquiring the justification.
//
// Origin: the #5220 dogfood walk on merged main. POST /bootstrap/admin returned
// 200 and the admin it created then got 401 on every login — a fresh install's
// first account permanently locked out, because the one-time migration that
// stamps pre-existing accounts had already run at boot. No unit test caught it:
// every test created users through a path already under consideration.
func (u *User) MarkEmailVerifiedNow() {
	now := time.Now()
	u.EmailVerifiedAt = &now
}

// RevokeSessions invalidates every token issued to a user before now.
//
// Call it wherever a credential changes hands. It is the whole revocation
// mechanism: the epoch is compared against each token's claim on every
// authenticated request, so this takes effect immediately and everywhere,
// including the paths nobody remembered to think about.
//
// The increment happens in SQL rather than read-modify-write in Go, so two
// concurrent revocations cannot land on the same value.
//
// It is per user and all-or-nothing. That is why logout does NOT call it — a
// single epoch cannot distinguish "this device" from "every device", and
// signing someone out of their phone because they logged out on their laptop
// would be a worse bug than the one this fixes. Per-session revocation needs
// per-session identity (a jti and a store) and is a separate design.
func RevokeSessions(db *gorm.DB, userID uint) error {
	return db.Model(&User{}).Where("id = ?", userID).
		Update("session_epoch", gorm.Expr("session_epoch + 1")).Error
}

// SessionEpochFor reads a user's current session epoch.
//
// Returns an error only when the read itself fails — a user row that has
// vanished is reported as an error, because a token for a user who no longer
// exists must not be honoured on the strength of a missing row.
func SessionEpochFor(db *gorm.DB, userID uint) (uint, error) {
	var user User
	if err := db.Select("session_epoch").Where("id = ?", userID).First(&user).Error; err != nil {
		return 0, err
	}
	return user.SessionEpoch, nil
}

// MigrateEmailVerified adds the email_verified_at column and, ONLY when the
// column did not exist beforehand, stamps every account that predates it.
//
// The guard is the whole point, and getting it wrong disables the verification
// gate on a timer. Sign-up creates accounts with email_verified_at NULL and
// LoginHandler refuses an unverified account, so the backfill's predicate
// (IS NULL) cannot distinguish a pre-existing admin account — which must be
// stamped — from a pending sign-up, which must not. Running it on every boot
// therefore marks every unverified sign-up verified at the next restart,
// including one created at an address the person does not own: the bot filter
// and the proof of address ownership are both bypassed, on a schedule, with
// nothing in the logs to say so.
//
// Keying on the column's prior absence makes the stamp genuinely one-time.
// Column existence is read through gorm's dialect rather than
// information_schema so the guard behaves identically under Postgres and the
// sqlite the tests use — a guard that could only run in production would be a
// guard nothing verifies. It mirrors the access_role column-existence branch
// already in server.AutoMigrate.
//
// Why the accounts need stamping at all: every account that exists today was
// created by an admin and has never seen a verification mail, so without this
// the deploy that adds the login check locks out every existing user, the
// operator included.
//
// created_at rather than now(): the address was trusted from the moment an
// admin made the account, and recording a verification at a time it did not
// happen would be a lie in the data.
//
// Origin: security review of aspirant-server PR #102 (system_3 finding #5226,
// severity high). The first version of this ran unconditionally inside
// AutoMigrate, and its own comment carried the mistake — "matches nothing for
// accounts created through sign-up, because those exist only after this point
// in the boot" is true within one boot and false across a restart.
func MigrateEmailVerified(db *gorm.DB) error {
	columnExisted := db.Dialect().HasColumn("users", "email_verified_at")

	if err := db.AutoMigrate(&User{}).Error; err != nil {
		return err
	}

	if columnExisted {
		// Not the first boot with this column. Every remaining NULL is an
		// account that has genuinely not verified its address, and leaving it
		// alone is the entire security property.
		return nil
	}

	return backfillEmailVerified(db)
}

// backfillEmailVerified stamps every account with no verification timestamp.
//
// Unexported deliberately: called correctly it runs exactly once, from
// MigrateEmailVerified, behind that function's column guard. Called from
// anywhere else it is the defect described above.
func backfillEmailVerified(db *gorm.DB) error {
	return db.Exec("UPDATE users SET email_verified_at = created_at WHERE email_verified_at IS NULL").Error
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
