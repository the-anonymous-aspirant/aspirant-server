package handlers

import (
	"errors"
	"log"
	"os"
	"time"

	"aspirant-online/server/data_models"
	"aspirant-online/server/storage"

	"github.com/jinzhu/gorm"
)

// Thirty days after an account is deleted, nothing personal remains on its
// row (system_3 #11591; the operator's answer c70522 on #11380). Deleting a
// user only soft-deletes the row, so until this ran the username, address,
// password hash, comment and avatar stayed in the table for good.
//
// Every line this file logs names a user by id and nothing else.

// StartIdentifierErasureSweep launches the single background goroutine: one
// pass at server start and then hourly. Nil db makes it a no-op, the same
// construction as the other Start* jobs.
func StartIdentifierErasureSweep(db *gorm.DB, assets storage.StorageBackend) {
	if db == nil {
		return
	}
	go func() {
		eraseDueIdentifiersSafely(db, assets)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			eraseDueIdentifiersSafely(db, assets)
		}
	}()
}

func eraseDueIdentifiersSafely(db *gorm.DB, assets storage.StorageBackend) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("identifier erasure sweep: recovered from panic: %v", r)
		}
	}()
	eraseDueIdentifiers(db, assets, time.Now().UTC())
}

// eraseDueIdentifiers is one pass, and returns how many accounts it erased.
// The first pass after this ships also erases accounts deleted more than
// thirty days before it: that is intended.
func eraseDueIdentifiers(db *gorm.DB, assets storage.StorageBackend, now time.Time) int {
	users, err := data_models.UsersDueForIdentifierErasure(db, now)
	if err != nil {
		log.Printf("identifier erasure sweep: listing accounts past their thirty days: %v", err)
		return 0
	}
	erased := 0
	for _, u := range users {
		// The picture first, then the row: a row erased while its blob is
		// still on disk would have lost the only pointer to that blob.
		if u.AvatarETag != "" && !avatarBlobSettled(db, assets, u.ID, u.AvatarETag) {
			continue
		}
		if err := data_models.EraseUserIdentifiers(db, u.ID, now); err != nil {
			log.Printf("identifier erasure sweep: erasing user %d: %v (retrying next pass)", u.ID, err)
			continue
		}
		erased++
	}
	if erased > 0 {
		log.Printf("identifier erasure sweep: erased the identifiers of %d deleted account(s)", erased)
	}
	return erased
}

// avatarBlobSettled reports whether the account's row may now be erased: its
// picture is gone from the store, or has to stay because another account
// still carries the same one. False leaves the row for the next pass.
//
// Known limit: the store is not transactional. A live account that uploads
// the identical picture between the count and the delete below would keep
// its pointer and lose the file. Nothing else in the server locks the store,
// and the window is the few milliseconds between two calls, once an hour.
func avatarBlobSettled(db *gorm.DB, assets storage.StorageBackend, userID uint, etag string) bool {
	held, err := data_models.AvatarETagHeldByAnother(db, userID, etag)
	if err != nil {
		log.Printf("identifier erasure sweep: checking who else holds the avatar of user %d: %v (retrying next pass)", userID, err)
		return false
	}
	if held {
		return true
	}
	if assets == nil {
		log.Printf("identifier erasure sweep: user %d has an avatar and no asset storage is configured; leaving the row for the next pass", userID)
		return false
	}
	if err := assets.Delete("avatars/" + etag); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("identifier erasure sweep: deleting the avatar of user %d: %v (retrying next pass)", userID, err)
		return false
	}
	return true
}
