package data_models

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newTokenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&UserToken{})
	t.Cleanup(func() { db.Close() })
	return db
}

// freezeClock pins tokenNow for the duration of a test and restores it after.
// Tests using it must not run in parallel — tokenNow is package state.
func freezeClock(t *testing.T, at time.Time) func(time.Time) {
	t.Helper()
	prev := tokenNow
	now := at
	tokenNow = func() time.Time { return now }
	t.Cleanup(func() { tokenNow = prev })
	return func(to time.Time) { now = to }
}

func baseTime() time.Time {
	return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
}

// --- issue / consume round trip --------------------------------------------

func TestIssueThenConsume(t *testing.T) {
	for _, purpose := range []string{PurposeVerifyEmail, PurposePasswordReset} {
		t.Run(purpose, func(t *testing.T) {
			db := newTokenTestDB(t)
			freezeClock(t, baseTime())

			plaintext, err := IssueUserToken(db, 7, purpose)
			if err != nil {
				t.Fatalf("IssueUserToken: %v", err)
			}
			if plaintext == "" {
				t.Fatal("issued an empty token")
			}

			tok, err := ConsumeUserToken(db, purpose, plaintext)
			if err != nil {
				t.Fatalf("ConsumeUserToken: %v", err)
			}
			if tok.UserID != 7 {
				t.Errorf("UserID = %d, want 7", tok.UserID)
			}
			if tok.Purpose != purpose {
				t.Errorf("Purpose = %q, want %q", tok.Purpose, purpose)
			}
			if tok.ConsumedAt == nil {
				t.Error("returned token is not marked consumed")
			}
		})
	}
}

// The plaintext must exist only in the caller's hand. Anyone reading the table
// — a backup, a log shipper, an operator at a psql prompt — must get digests.
func TestIssuedPlaintextIsNeverStored(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	plaintext, err := IssueUserToken(db, 7, PurposePasswordReset)
	if err != nil {
		t.Fatalf("IssueUserToken: %v", err)
	}

	var rows []UserToken
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].TokenHash == plaintext {
		t.Fatal("the plaintext token was stored verbatim")
	}
	if strings.Contains(rows[0].TokenHash, plaintext) {
		t.Fatal("the stored hash contains the plaintext")
	}
	if rows[0].TokenHash != hashToken(plaintext) {
		t.Error("stored value is not the digest of the issued token")
	}
	// A 32-byte digest, hex-encoded.
	if len(rows[0].TokenHash) != 64 {
		t.Errorf("TokenHash length = %d, want 64 hex chars", len(rows[0].TokenHash))
	}
}

func TestIssuedTokensAreDistinct(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		p, err := IssueUserToken(db, uint(i+1), PurposeVerifyEmail)
		if err != nil {
			t.Fatalf("IssueUserToken: %v", err)
		}
		if seen[p] {
			t.Fatalf("issued a duplicate token at iteration %d", i)
		}
		seen[p] = true
	}
}

// --- the four indistinguishable failures ------------------------------------

// Not-found, expired, already-consumed and wrong-purpose must be one error.
// Telling them apart hands a guesser most of the work of finding a live token:
// "this one existed but expired" says the guess was structurally right.
func TestEveryFailureIsTheSameError(t *testing.T) {
	t.Run("never existed", func(t *testing.T) {
		db := newTokenTestDB(t)
		freezeClock(t, baseTime())

		_, err := ConsumeUserToken(db, PurposeVerifyEmail, "no-such-token")
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("err = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		db := newTokenTestDB(t)
		freezeClock(t, baseTime())

		if _, err := ConsumeUserToken(db, PurposeVerifyEmail, ""); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("err = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		db := newTokenTestDB(t)
		advance := freezeClock(t, baseTime())

		plaintext, err := IssueUserToken(db, 7, PurposePasswordReset)
		if err != nil {
			t.Fatalf("IssueUserToken: %v", err)
		}
		advance(baseTime().Add(time.Hour + time.Second))

		if _, err := ConsumeUserToken(db, PurposePasswordReset, plaintext); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("err = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("already consumed", func(t *testing.T) {
		db := newTokenTestDB(t)
		freezeClock(t, baseTime())

		plaintext, err := IssueUserToken(db, 7, PurposeVerifyEmail)
		if err != nil {
			t.Fatalf("IssueUserToken: %v", err)
		}
		if _, err := ConsumeUserToken(db, PurposeVerifyEmail, plaintext); err != nil {
			t.Fatalf("first consume: %v", err)
		}
		if _, err := ConsumeUserToken(db, PurposeVerifyEmail, plaintext); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("replay err = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("wrong purpose", func(t *testing.T) {
		db := newTokenTestDB(t)
		freezeClock(t, baseTime())

		plaintext, err := IssueUserToken(db, 7, PurposeVerifyEmail)
		if err != nil {
			t.Fatalf("IssueUserToken: %v", err)
		}
		// A verification token presented to the password-reset endpoint must
		// not work: otherwise the weaker flow mints credentials for the
		// stronger one.
		if _, err := ConsumeUserToken(db, PurposePasswordReset, plaintext); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("err = %v, want ErrTokenInvalid", err)
		}
		// And it must still be spendable on its own endpoint — the failed
		// attempt must not have consumed it.
		if _, err := ConsumeUserToken(db, PurposeVerifyEmail, plaintext); err != nil {
			t.Fatalf("token was burned by the wrong-purpose attempt: %v", err)
		}
	})

	t.Run("unknown purpose", func(t *testing.T) {
		db := newTokenTestDB(t)
		freezeClock(t, baseTime())

		if _, err := ConsumeUserToken(db, "not_a_purpose", "whatever"); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("err = %v, want ErrTokenInvalid", err)
		}
	})
}

// Expiry is exclusive at the boundary: a token whose ExpiresAt is exactly now
// is spent.
func TestExpiryBoundary(t *testing.T) {
	db := newTokenTestDB(t)
	advance := freezeClock(t, baseTime())

	plaintext, err := IssueUserToken(db, 7, PurposePasswordReset)
	if err != nil {
		t.Fatalf("IssueUserToken: %v", err)
	}

	advance(baseTime().Add(time.Hour).Add(-time.Nanosecond))
	if _, err := ConsumeUserToken(db, PurposePasswordReset, plaintext); err != nil {
		t.Fatalf("token rejected one nanosecond before expiry: %v", err)
	}
}

func TestExpiryBoundaryExactlyAtExpiry(t *testing.T) {
	db := newTokenTestDB(t)
	advance := freezeClock(t, baseTime())

	plaintext, err := IssueUserToken(db, 7, PurposePasswordReset)
	if err != nil {
		t.Fatalf("IssueUserToken: %v", err)
	}

	advance(baseTime().Add(time.Hour))
	if _, err := ConsumeUserToken(db, PurposePasswordReset, plaintext); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want the token to be expired at exactly ExpiresAt", err)
	}
}

// --- the policy table -------------------------------------------------------

func TestPolicyTTLs(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	cases := map[string]time.Duration{
		PurposeVerifyEmail:   24 * time.Hour,
		PurposePasswordReset: time.Hour,
	}
	for purpose, wantTTL := range cases {
		if _, err := IssueUserToken(db, 7, purpose); err != nil {
			t.Fatalf("IssueUserToken(%s): %v", purpose, err)
		}
		var row UserToken
		if err := db.Where("purpose = ?", purpose).Order("id desc").First(&row).Error; err != nil {
			t.Fatalf("reading back %s: %v", purpose, err)
		}
		if got := row.ExpiresAt.Sub(baseTime()); got != wantTTL {
			t.Errorf("%s TTL = %v, want %v", purpose, got, wantTTL)
		}
	}
}

// A typo'd purpose must not quietly get some default lifetime — that is how a
// password reset ends up living for a day.
func TestIssueRejectsUnknownPurpose(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	_, err := IssueUserToken(db, 7, "verify-email")
	if !errors.Is(err, ErrUnknownTokenPurpose) {
		t.Fatalf("err = %v, want ErrUnknownTokenPurpose", err)
	}
	var count int
	db.Model(&UserToken{}).Count(&count)
	if count != 0 {
		t.Errorf("a row was written for an unknown purpose (%d rows)", count)
	}
}

func TestIssueRejectsZeroUser(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	if _, err := IssueUserToken(db, 0, PurposeVerifyEmail); err == nil {
		t.Fatal("issued a token with no user")
	}
}

// The asymmetry that justifies the policy table: a new reset link retires the
// old one, a new verification link does not.
func TestResetIssueRetiresPreviousResetTokens(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	first, err := IssueUserToken(db, 7, PurposePasswordReset)
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	second, err := IssueUserToken(db, 7, PurposePasswordReset)
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}

	if _, err := ConsumeUserToken(db, PurposePasswordReset, first); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("the superseded reset token still works: %v", err)
	}
	if _, err := ConsumeUserToken(db, PurposePasswordReset, second); err != nil {
		t.Errorf("the newest reset token was rejected: %v", err)
	}
}

func TestVerifyIssueKeepsPreviousVerifyTokens(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	first, err := IssueUserToken(db, 7, PurposeVerifyEmail)
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if _, err := IssueUserToken(db, 7, PurposeVerifyEmail); err != nil {
		t.Fatalf("second issue: %v", err)
	}

	// Re-requesting a verification mail usually means the first never arrived;
	// if both turn up, clicking the older one must still work.
	if _, err := ConsumeUserToken(db, PurposeVerifyEmail, first); err != nil {
		t.Errorf("the earlier verification token was retired: %v", err)
	}
}

// Retiring siblings is per user and per purpose.
func TestResetIssueDoesNotTouchOtherUsersOrPurposes(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	otherUser, err := IssueUserToken(db, 8, PurposePasswordReset)
	if err != nil {
		t.Fatalf("issue for other user: %v", err)
	}
	sameUserVerify, err := IssueUserToken(db, 7, PurposeVerifyEmail)
	if err != nil {
		t.Fatalf("issue verify: %v", err)
	}

	if _, err := IssueUserToken(db, 7, PurposePasswordReset); err != nil {
		t.Fatalf("issue reset: %v", err)
	}

	if _, err := ConsumeUserToken(db, PurposePasswordReset, otherUser); err != nil {
		t.Errorf("another user's reset token was retired: %v", err)
	}
	if _, err := ConsumeUserToken(db, PurposeVerifyEmail, sameUserVerify); err != nil {
		t.Errorf("the same user's verification token was retired: %v", err)
	}
}

// --- single use -------------------------------------------------------------

// A double-clicked reset link is the ordinary behaviour of an impatient user on
// a slow connection, not an exotic race. Exactly one consumer may win.
//
// What this proves and what it does not: the test drives concurrent goroutines,
// but the sqlite driver serialises them, so it exercises the conditional update
// (`WHERE consumed_at IS NULL`, zero rows affected treated as failure) rather
// than Postgres row locking. That conditional is the guarantee — it is atomic
// in either engine, and a read-then-write in Go would fail this test under
// serialisation too.
func TestConcurrentConsumeYieldsExactlyOneSuccess(t *testing.T) {
	db := newTokenTestDB(t)
	freezeClock(t, baseTime())

	plaintext, err := IssueUserToken(db, 7, PurposePasswordReset)
	if err != nil {
		t.Fatalf("IssueUserToken: %v", err)
	}

	const consumers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		otherErrs []error
	)
	wg.Add(consumers)
	for i := 0; i < consumers; i++ {
		go func() {
			defer wg.Done()
			_, err := ConsumeUserToken(db, PurposePasswordReset, plaintext)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrTokenInvalid):
			default:
				otherErrs = append(otherErrs, err)
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1", successes)
	}
	if len(otherErrs) > 0 {
		t.Errorf("unexpected errors: %v", otherErrs)
	}
}

// --- purge ------------------------------------------------------------------

func TestPurgeRemovesOnlyDeadRows(t *testing.T) {
	db := newTokenTestDB(t)
	advance := freezeClock(t, baseTime())

	expiring, err := IssueUserToken(db, 7, PurposePasswordReset) // 1h TTL
	if err != nil {
		t.Fatalf("issue expiring: %v", err)
	}
	_ = expiring
	live, err := IssueUserToken(db, 8, PurposeVerifyEmail) // 24h TTL
	if err != nil {
		t.Fatalf("issue live: %v", err)
	}

	// Two hours later the reset token is dead and the verification token is not.
	advance(baseTime().Add(2 * time.Hour))

	n, err := PurgeExpiredUserTokens(db, 30*time.Minute)
	if err != nil {
		t.Fatalf("PurgeExpiredUserTokens: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}
	if _, err := ConsumeUserToken(db, PurposeVerifyEmail, live); err != nil {
		t.Errorf("the still-live token was purged: %v", err)
	}
}

func TestPurgeKeepsRecentlyConsumedRowsWithinGrace(t *testing.T) {
	db := newTokenTestDB(t)
	advance := freezeClock(t, baseTime())

	plaintext, err := IssueUserToken(db, 7, PurposeVerifyEmail)
	if err != nil {
		t.Fatalf("IssueUserToken: %v", err)
	}
	if _, err := ConsumeUserToken(db, PurposeVerifyEmail, plaintext); err != nil {
		t.Fatalf("consume: %v", err)
	}

	advance(baseTime().Add(5 * time.Minute))
	n, err := PurgeExpiredUserTokens(db, time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredUserTokens: %v", err)
	}
	if n != 0 {
		t.Errorf("purged %d rows inside the grace window, want 0", n)
	}
}
