package data_models

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"
)

// Single-use expiring user tokens (system_3 epic #5113, subtask #5219).
//
// One model backs both email verification and password recovery. That is a
// deliberate collapse, not an accident of convenience: the two differ only in
// how long the token lives and whether issuing one retires the previous one.
// Everything that is easy to get wrong — that the plaintext is never stored,
// that a token is consumed exactly once, that expiry is checked before use, and
// that a failure tells the caller nothing about which of those it was — is
// identical. Two models would have meant two chances to get single-use wrong.
// The variation lives in the policy table below rather than in a second file.
//
// This layer owns storage and lifecycle only. What a token authorises, what the
// mail says, and which endpoint consumes it belong to #5220 and #5221.

// Token purposes. A purpose is stored on the row and supplied again at consume
// time, so a token minted for one flow cannot be spent on the other.
const (
	PurposeVerifyEmail   = "verify_email"
	PurposePasswordReset = "password_reset"
)

// tokenBytes is the size of the random token before encoding.
//
// 32 bytes is 256 bits of entropy from crypto/rand. These tokens travel in a
// URL and are the sole credential the verification and recovery endpoints
// accept, so they must be infeasible to guess with unlimited offline attempts;
// at this size the rate limits on the consuming endpoints are a convenience
// rather than the thing standing between an attacker and an account.
const tokenBytes = 32

// tokenPolicy is one row of the purpose table.
type tokenPolicy struct {
	purpose string
	ttl     time.Duration
	// invalidatesSiblingsOnIssue retires a user's other unconsumed tokens of
	// the same purpose when a new one is issued.
	invalidatesSiblingsOnIssue bool
}

// tokenPolicies is the ordered (purpose, strategy) table that carries every
// difference between the two flows.
//
// password_reset retires its siblings and verify_email does not, and the
// asymmetry is intentional. Requesting a password reset is what a person does
// when they suspect they have lost control of the account, so the older link —
// which may be the one sitting in an attacker's hands — must stop working the
// moment a new one is asked for. A verification link carries no such signal:
// re-requesting one usually means the first never arrived, and killing it would
// break the common case where both eventually turn up and the user clicks the
// older mail.
//
// The one-hour reset TTL versus twenty-four hours for verification follows the
// same reasoning: a reset link is a live credential for an existing account, a
// verification link only finishes creating one.
var tokenPolicies = []tokenPolicy{
	{purpose: PurposeVerifyEmail, ttl: 24 * time.Hour, invalidatesSiblingsOnIssue: false},
	{purpose: PurposePasswordReset, ttl: time.Hour, invalidatesSiblingsOnIssue: true},
}

// tokenNow is the clock. Tests substitute it so expiry can be exercised without
// sleeping; production never reassigns it.
var tokenNow = time.Now

var (
	// ErrUnknownTokenPurpose reports a purpose with no policy row. It is
	// returned from Issue only — a programming error, caught at the call site.
	ErrUnknownTokenPurpose = errors.New("user_token: unknown token purpose")

	// ErrTokenInvalid is the single failure a consumer ever sees.
	//
	// Not-found, expired, already-consumed and minted-for-the-other-purpose all
	// return exactly this. Separating them would let someone submitting guesses
	// learn which ones were structurally right — that a token existed but had
	// expired says the guess was real, which is most of the work of finding a
	// live one. The handlers above must not decorate it either.
	ErrTokenInvalid = errors.New("user_token: token is invalid, expired, or already used")
)

// UserToken is one issued credential.
//
// TokenHash holds a SHA-256 digest; the plaintext is returned to the caller by
// Issue and never written anywhere. Whoever reads this table — a backup, a log
// shipper, an operator with a psql prompt — gets digests, not working reset
// links.
//
// SHA-256 rather than bcrypt is correct for this input and would be wrong for a
// password. Password hashing is slow on purpose because a human-chosen password
// sits in a searchable space; a 256-bit random token does not, so there is no
// dictionary to slow down and the cost would buy nothing while making every
// consume noticeably slower.
type UserToken struct {
	gorm.Model
	UserID    uint   `gorm:"not null;index"`
	Purpose   string `gorm:"not null;index"`
	TokenHash string `gorm:"not null;unique_index"`
	ExpiresAt time.Time
	// ConsumedAt is nil until the token is spent. A pointer, so "unused" is a
	// SQL NULL that the conditional update in Consume can test.
	ConsumedAt *time.Time
}

// TableName pins the table name; GORM's pluraliser would give the same answer,
// but the schema is a migration surface and should not depend on it.
func (UserToken) TableName() string { return "user_tokens" }

// policyFor returns the policy row for a purpose.
func policyFor(purpose string) (tokenPolicy, bool) {
	for _, p := range tokenPolicies {
		if p.purpose == purpose {
			return p, true
		}
	}
	return tokenPolicy{}, false
}

// hashToken digests a plaintext token for storage and lookup.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// IssueUserToken mints a token for a user and returns the plaintext.
//
// The plaintext is returned exactly once and is not recoverable afterwards: it
// is the caller's only chance to put it in a mail. An unknown purpose is
// refused rather than defaulting to some TTL, because a typo'd purpose that
// silently got a twenty-four-hour lifetime on a password reset is precisely the
// kind of quiet weakening this table exists to prevent.
func IssueUserToken(db *gorm.DB, userID uint, purpose string) (string, error) {
	policy, ok := policyFor(purpose)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTokenPurpose, purpose)
	}
	if userID == 0 {
		return "", errors.New("user_token: refusing to issue a token with no user")
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing is not recoverable by retrying, and issuing a
		// guessable token would be worse than issuing none.
		return "", fmt.Errorf("user_token: generating token: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	now := tokenNow()

	tx := db.Begin()
	if tx.Error != nil {
		return "", fmt.Errorf("user_token: begin: %w", tx.Error)
	}

	if policy.invalidatesSiblingsOnIssue {
		// Retire the user's other live tokens of this purpose in the same
		// transaction as the new one, so there is never a moment where both the
		// old and the new link work.
		if err := tx.Model(&UserToken{}).
			Where("user_id = ? AND purpose = ? AND consumed_at IS NULL", userID, purpose).
			Update("consumed_at", now).Error; err != nil {
			tx.Rollback()
			return "", fmt.Errorf("user_token: retiring previous tokens: %w", err)
		}
	}

	token := UserToken{
		UserID:    userID,
		Purpose:   purpose,
		TokenHash: hashToken(plaintext),
		ExpiresAt: now.Add(policy.ttl),
	}
	if err := tx.Create(&token).Error; err != nil {
		tx.Rollback()
		return "", fmt.Errorf("user_token: creating token: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return "", fmt.Errorf("user_token: commit: %w", err)
	}
	return plaintext, nil
}

// ConsumeUserToken spends a token and returns the row it spent.
//
// Every failure returns ErrTokenInvalid — see that variable's comment.
//
// Single use is enforced by the database, not by this function. The update is
// conditioned on consumed_at IS NULL and a zero row count is treated as
// failure, so two concurrent consumers of the same token produce exactly one
// success whatever their interleaving. A read-then-write in Go would let a
// double-clicked reset link through twice, which is not an exotic race but the
// ordinary behaviour of an impatient user on a slow connection.
func ConsumeUserToken(db *gorm.DB, purpose, plaintext string) (*UserToken, error) {
	if plaintext == "" {
		return nil, ErrTokenInvalid
	}
	if _, ok := policyFor(purpose); !ok {
		return nil, ErrTokenInvalid
	}

	now := tokenNow()

	tx := db.Begin()
	if tx.Error != nil {
		return nil, ErrTokenInvalid
	}

	// Look the row up by the digest of what was presented. The database does
	// the matching against a unique index, so no secret is compared byte-wise
	// in this process; there is no string comparison here for a timing side
	// channel to observe. (Constant-time comparison is the right control when a
	// secret is checked in application code — it is not what is happening here,
	// and adding it would suggest a defence that is not the one in force.)
	var token UserToken
	err := tx.Where("token_hash = ? AND purpose = ?", hashToken(plaintext), purpose).
		First(&token).Error
	if err != nil {
		tx.Rollback()
		return nil, ErrTokenInvalid
	}

	if token.ConsumedAt != nil || !now.Before(token.ExpiresAt) {
		tx.Rollback()
		return nil, ErrTokenInvalid
	}

	res := tx.Model(&UserToken{}).
		Where("id = ? AND consumed_at IS NULL", token.ID).
		Update("consumed_at", now)
	if res.Error != nil {
		tx.Rollback()
		return nil, ErrTokenInvalid
	}
	if res.RowsAffected == 0 {
		// Another consumer won. The read above saw an unconsumed row; this is
		// the only place that difference is visible, and it is a failure.
		tx.Rollback()
		return nil, ErrTokenInvalid
	}

	if err := tx.Commit().Error; err != nil {
		return nil, ErrTokenInvalid
	}

	token.ConsumedAt = &now
	return &token, nil
}

// PurgeExpiredUserTokens deletes rows that can no longer be used.
//
// Retention, not hygiene: a consumed or expired token row is a record that a
// particular account requested a password reset at a particular time, which is
// not something worth keeping once the row can never authorise anything. The
// grace period keeps recently-spent rows briefly so that a user who clicks a
// link twice gets ErrTokenInvalid rather than a bare not-found, which are the
// same to them but different to anyone reading the logs.
//
// No caller yet — the consuming endpoints land in #5220 and #5221, and this is
// here so the table does not grow without a plan.
func PurgeExpiredUserTokens(db *gorm.DB, grace time.Duration) (int64, error) {
	cutoff := tokenNow().Add(-grace)
	res := db.Unscoped().
		Where("expires_at < ? OR (consumed_at IS NOT NULL AND consumed_at < ?)", cutoff, cutoff).
		Delete(&UserToken{})
	if res.Error != nil {
		return 0, fmt.Errorf("user_token: purging: %w", res.Error)
	}
	return res.RowsAffected, nil
}
