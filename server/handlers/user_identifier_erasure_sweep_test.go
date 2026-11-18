package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/storage"

	"github.com/jinzhu/gorm"
)

// Thirty days after deletion a user row keeps nothing personal (system_3
// #11591, Part B of #11471). Every value here is invented.

var erasureNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return erasureNow.Add(-time.Duration(n) * 24 * time.Hour) }

func newErasureStore(t *testing.T) *storage.LocalStorage {
	t.Helper()
	store, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("asset store: %v", err)
	}
	return store
}

// seedErasureUser creates an account with every personal field filled. A
// non-nil deletedAt soft-deletes it at that time. picture, when not empty, is
// stored as the avatar and its etag returned.
func seedErasureUser(t *testing.T, db *gorm.DB, store *storage.LocalStorage, n int, deletedAt *time.Time, picture string) (data_models.User, string) {
	t.Helper()
	u := data_models.User{
		Username: fmt.Sprintf("invented-person-%d", n),
		Email:    fmt.Sprintf("person%d@example.test", n),
		Password: fmt.Sprintf("invented-hash-%d", n),
		Comment:  fmt.Sprintf("invented comment %d", n),
	}
	mustCreate(t, db, &u)
	etag := ""
	if picture != "" {
		etag = fmt.Sprintf("invented-etag-%s", picture)
		if err := store.Put("avatars/"+etag, strings.NewReader("invented picture "+picture)); err != nil {
			t.Fatalf("store avatar: %v", err)
		}
		if err := db.Model(&data_models.User{}).Where("id = ?", u.ID).Update("AvatarETag", etag).Error; err != nil {
			t.Fatalf("set avatar: %v", err)
		}
	}
	if deletedAt != nil {
		if err := db.Unscoped().Model(&data_models.User{}).Where("id = ?", u.ID).UpdateColumn("deleted_at", *deletedAt).Error; err != nil {
			t.Fatalf("backdate delete: %v", err)
		}
	}
	return readErasureUser(t, db, u.ID), etag
}

func readErasureUser(t *testing.T, db *gorm.DB, id uint) data_models.User {
	t.Helper()
	var u data_models.User
	if err := db.Unscoped().First(&u, id).Error; err != nil {
		t.Fatalf("read user %d: %v", id, err)
	}
	return u
}

func blobExists(store *storage.LocalStorage, etag string) bool {
	_, err := store.Stat("avatars/" + etag)
	return err == nil
}

// personalFields is the row's personal content, by name, so a field left out
// of the erase fails as itself.
func personalFields(u data_models.User) map[string]string {
	return map[string]string{"Username": u.Username, "Email": u.Email, "Password": u.Password, "Comment": u.Comment, "AvatarETag": u.AvatarETag}
}

func TestIdentifierErasure_ErasesAnAccountDeleted31DaysAgo(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	deleted := daysAgo(31)
	before, etag := seedErasureUser(t, db, store, 1, &deleted, "a")
	// Positive control: every field is filled before the pass, or the checks
	// below could pass on a row that was empty to begin with.
	for name, value := range personalFields(before) {
		if value == "" {
			t.Fatalf("seed: %s is empty before the pass", name)
		}
	}
	if !blobExists(store, etag) {
		t.Fatal("seed: the avatar blob is not in the store")
	}

	if n := eraseDueIdentifiers(db, store, erasureNow); n != 1 {
		t.Fatalf("erased %d account(s), want 1", n)
	}

	after := readErasureUser(t, db, before.ID)
	want := map[string]string{"Username": fmt.Sprintf("deleted-%d", before.ID), "Email": "", "Password": "", "Comment": "", "AvatarETag": ""}
	for name, value := range personalFields(after) {
		if value != want[name] {
			t.Errorf("%s = %q after the pass, want %q", name, value, want[name])
		}
	}
	if after.IdentifiersErasedAt == nil || !after.IdentifiersErasedAt.Equal(erasureNow) {
		t.Errorf("IdentifiersErasedAt = %v, want %v", after.IdentifiersErasedAt, erasureNow)
	}
	if after.DeletedAt == nil || !after.DeletedAt.Equal(deleted) {
		t.Errorf("DeletedAt = %v, want it left at %v: the row stays soft-deleted", after.DeletedAt, deleted)
	}
	if blobExists(store, etag) {
		t.Error("the avatar blob is still in the store")
	}
}

func TestIdentifierErasure_LeavesARecentDeleteAndALiveAccount(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	recent := daysAgo(29)
	recentUser, recentEtag := seedErasureUser(t, db, store, 1, &recent, "a")
	liveUser, liveEtag := seedErasureUser(t, db, store, 2, nil, "b")

	if n := eraseDueIdentifiers(db, store, erasureNow); n != 0 {
		t.Fatalf("erased %d account(s), want 0", n)
	}
	for _, tc := range []struct {
		what   string
		before data_models.User
		etag   string
	}{{"deleted 29 days ago", recentUser, recentEtag}, {"live", liveUser, liveEtag}} {
		after := readErasureUser(t, db, tc.before.ID)
		for name, value := range personalFields(after) {
			if value != personalFields(tc.before)[name] {
				t.Errorf("%s: %s changed to %q", tc.what, name, value)
			}
		}
		if after.IdentifiersErasedAt != nil {
			t.Errorf("%s: marked erased at %v", tc.what, after.IdentifiersErasedAt)
		}
		if !blobExists(store, tc.etag) {
			t.Errorf("%s: the avatar blob is gone", tc.what)
		}
	}
}

func TestIdentifierErasure_ASecondPassChangesNothing(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	deleted := daysAgo(40)
	u, _ := seedErasureUser(t, db, store, 1, &deleted, "a")
	if n := eraseDueIdentifiers(db, store, erasureNow); n != 1 {
		t.Fatalf("first pass erased %d, want 1", n)
	}
	first := readErasureUser(t, db, u.ID)

	later := erasureNow.Add(time.Hour)
	if n := eraseDueIdentifiers(db, store, later); n != 0 {
		t.Errorf("second pass erased %d, want 0", n)
	}
	second := readErasureUser(t, db, u.ID)
	if !second.IdentifiersErasedAt.Equal(*first.IdentifiersErasedAt) {
		t.Errorf("the marker moved from %v to %v", first.IdentifiersErasedAt, second.IdentifiersErasedAt)
	}
	if second.Username != first.Username || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("the row was written again: %q at %v, was %q at %v", second.Username, second.UpdatedAt, first.Username, first.UpdatedAt)
	}
}

// The store is content-addressed: two accounts with the same picture share
// one blob, and it goes only with the last account that carries it.
func TestIdentifierErasure_KeepsABlobAnotherAccountStillCarries(t *testing.T) {
	old := daysAgo(31)
	recent := daysAgo(5)
	for _, tc := range []struct {
		name        string
		holderGone  *time.Time
		holderLabel string
	}{
		{"a live account", nil, "live"},
		{"an account deleted too recently to be erased", &recent, "recently deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupDeleteContentDB(t)
			store := newErasureStore(t)
			due, etag := seedErasureUser(t, db, store, 1, &old, "shared")
			holder, holderEtag := seedErasureUser(t, db, store, 2, tc.holderGone, "shared")
			if etag != holderEtag {
				t.Fatalf("seed: the two accounts do not share one blob (%q, %q)", etag, holderEtag)
			}

			if n := eraseDueIdentifiers(db, store, erasureNow); n != 1 {
				t.Fatalf("erased %d account(s), want 1", n)
			}
			if got := readErasureUser(t, db, due.ID); got.AvatarETag != "" || got.IdentifiersErasedAt == nil {
				t.Errorf("the due account was not erased: etag %q, marker %v", got.AvatarETag, got.IdentifiersErasedAt)
			}
			if !blobExists(store, etag) {
				t.Errorf("the blob is gone while the %s account still carries it", tc.holderLabel)
			}
			if got := readErasureUser(t, db, holder.ID); got.AvatarETag != etag {
				t.Errorf("the %s account's avatar pointer changed to %q", tc.holderLabel, got.AvatarETag)
			}
		})
	}
}

func TestIdentifierErasure_TwoDueAccountsSharingAPictureBothGoAndSoDoesTheBlob(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	old := daysAgo(31)
	first, etag := seedErasureUser(t, db, store, 1, &old, "shared")
	second, _ := seedErasureUser(t, db, store, 2, &old, "shared")

	if n := eraseDueIdentifiers(db, store, erasureNow); n != 2 {
		t.Fatalf("erased %d account(s), want 2", n)
	}
	for _, id := range []uint{first.ID, second.ID} {
		if got := readErasureUser(t, db, id); got.AvatarETag != "" || got.IdentifiersErasedAt == nil {
			t.Errorf("user %d was not erased: etag %q, marker %v", id, got.AvatarETag, got.IdentifiersErasedAt)
		}
	}
	if blobExists(store, etag) {
		t.Error("the shared blob outlived both accounts")
	}
}

// refusingStore is a store whose Delete always fails.
type refusingStore struct{ *storage.LocalStorage }

func (refusingStore) Delete(string) error { return errors.New("invented: the disk refused") }

func TestIdentifierErasure_AFailedBlobDeleteLeavesTheRowForTheNextPass(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	old := daysAgo(31)
	before, etag := seedErasureUser(t, db, store, 1, &old, "a")
	// A second due account with no picture: one refused blob must not stop
	// the accounts behind it.
	plain, _ := seedErasureUser(t, db, store, 2, &old, "")

	if n := eraseDueIdentifiers(db, refusingStore{store}, erasureNow); n != 1 {
		t.Fatalf("erased %d account(s), want 1 (the one with no picture)", n)
	}
	after := readErasureUser(t, db, before.ID)
	for name, value := range personalFields(after) {
		if value != personalFields(before)[name] {
			t.Errorf("%s changed to %q although the blob could not be deleted", name, value)
		}
	}
	if after.IdentifiersErasedAt != nil {
		t.Errorf("marked erased at %v although the blob is still there", after.IdentifiersErasedAt)
	}
	if !blobExists(store, etag) {
		t.Fatal("the refusing store deleted the blob")
	}
	if got := readErasureUser(t, db, plain.ID); got.IdentifiersErasedAt == nil {
		t.Error("the account with no picture was not erased")
	}

	// The next pass, with a store that works, finishes it.
	if n := eraseDueIdentifiers(db, store, erasureNow.Add(time.Hour)); n != 1 {
		t.Fatalf("the next pass erased %d account(s), want 1", n)
	}
	if got := readErasureUser(t, db, before.ID); got.Email != "" || got.IdentifiersErasedAt == nil || blobExists(store, etag) {
		t.Errorf("the next pass did not finish: email %q, marker %v, blob present %v", got.Email, got.IdentifiersErasedAt, blobExists(store, etag))
	}
}

func TestIdentifierErasure_ABlobAlreadyGoneCountsAsDeleted(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	old := daysAgo(31)
	u, etag := seedErasureUser(t, db, store, 1, &old, "a")
	if err := store.Delete("avatars/" + etag); err != nil {
		t.Fatalf("remove the blob by hand: %v", err)
	}
	if n := eraseDueIdentifiers(db, store, erasureNow); n != 1 {
		t.Fatalf("erased %d account(s), want 1", n)
	}
	if got := readErasureUser(t, db, u.ID); got.IdentifiersErasedAt == nil {
		t.Error("an avatar pointer with no blob behind it stopped the erase")
	}
}

// With no store at all an account that has a picture waits, and one that has
// none is erased.
func TestIdentifierErasure_WithNoStoreOnlyAccountsWithoutAPictureAreErased(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	old := daysAgo(31)
	withPicture, _ := seedErasureUser(t, db, store, 1, &old, "a")
	without, _ := seedErasureUser(t, db, store, 2, &old, "")
	if n := eraseDueIdentifiers(db, nil, erasureNow); n != 1 {
		t.Fatalf("erased %d account(s), want 1", n)
	}
	if got := readErasureUser(t, db, withPicture.ID); got.IdentifiersErasedAt != nil {
		t.Error("an account with a picture was erased with no store to remove the picture from")
	}
	if got := readErasureUser(t, db, without.ID); got.IdentifiersErasedAt == nil {
		t.Error("an account with no picture was not erased")
	}
}

func TestIdentifierErasure_LogsIdsAndNoUsernameOrAddress(t *testing.T) {
	db := setupDeleteContentDB(t)
	store := newErasureStore(t)
	old := daysAgo(31)
	erased, _ := seedErasureUser(t, db, store, 1, &old, "a")
	refused, _ := seedErasureUser(t, db, store, 2, &old, "b")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	// One pass that erases, and one where the blob delete fails, so both the
	// success line and the failure line are in the capture.
	eraseDueIdentifiers(db, refusingStore{store}, erasureNow)
	eraseDueIdentifiers(db, store, erasureNow.Add(time.Hour))
	out := buf.String()

	// Positive control: the capture holds the sweep's lines, with an id in them.
	if !strings.Contains(out, "identifier erasure sweep") || !strings.Contains(out, fmt.Sprintf("user %d", erased.ID)) {
		t.Fatalf("the capture does not hold the sweep's lines: %q", out)
	}
	for _, u := range []data_models.User{erased, refused} {
		for name, value := range map[string]string{"username": u.Username, "address": u.Email, "comment": u.Comment, "password hash": u.Password} {
			if strings.Contains(out, value) {
				t.Errorf("the log carries a %s (%q): %q", name, value, out)
			}
		}
	}
}

// The placeholder is built from the id and nothing else.
func TestErasedUsername(t *testing.T) {
	if got := data_models.ErasedUsername(42); got != "deleted-42" {
		t.Errorf("ErasedUsername(42) = %q", got)
	}
}
