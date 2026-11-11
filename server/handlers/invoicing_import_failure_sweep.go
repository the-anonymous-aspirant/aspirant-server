package handlers

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// Deletion is guaranteed two ways (security ruling c65222 §3): the DB-row
// path (a tracked copy past its window) and a directory sweep keyed on file
// mtime alone, so a lost row or a failed transaction can never strand a
// bank document past the window. Runs at server start and then hourly.

// StartImportFailureSweep launches the single background goroutine. Nil db
// (tests, or a boot path without one) makes it a no-op, same construction as
// the other Start* jobs in this file.
func StartImportFailureSweep(db *gorm.DB) {
	if db == nil {
		return
	}
	go func() {
		sweepImportFailuresSafely(db)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			sweepImportFailuresSafely(db)
		}
	}()
}

func sweepImportFailuresSafely(db *gorm.DB) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("import failure sweep: recovered from panic: %v", r)
		}
	}()
	sweepImportFailures(db)
}

func sweepImportFailures(db *gorm.DB) {
	window := importFailureWindow()
	cutoff := time.Now().UTC().Add(-window)

	deleteTrackedCopiesPast(db, cutoff)
	deleteOrphanFilesPast(cutoff)
	pruneOldRows(db)
}

// deleteTrackedCopiesPast removes the on-disk copy of every row past the
// window that still has one, and stamps CopyDeletedAt. A failed delete is
// logged and left for the next pass, never marked done (security ruling
// c65222 §3).
func deleteTrackedCopiesPast(db *gorm.DB, cutoff time.Time) {
	var rows []data_models.InvoicingImportFailure
	if err := db.Where("copy_path != '' AND copy_deleted_at IS NULL AND created_at < ?", cutoff).Find(&rows).Error; err != nil {
		log.Printf("import failure sweep: listing copies past window: %v", err)
		return
	}
	dir := importFailureDir()
	if dir == "" {
		// Never stamp a copy deleted that this pass did not delete: a deploy
		// that drops the variable but keeps the mount would otherwise mark
		// copies gone while they sit on the host (security review
		// non-blocking item 1 on #10536, c65539).
		if len(rows) > 0 {
			log.Printf("import failure sweep: %d copies past their window but IMPORT_FAILURE_COPIES_DIR is unset; leaving them unstamped", len(rows))
		}
		return
	}
	for _, row := range rows {
		if err := os.Remove(filepath.Join(dir, row.CopyPath)); err != nil && !os.IsNotExist(err) {
			log.Printf("import failure sweep: deleting copy of row %d: %v (retrying next pass)", row.ID, err)
			continue
		}
		now := time.Now().UTC()
		if err := db.Model(&data_models.InvoicingImportFailure{}).Where("id = ?", row.ID).
			UpdateColumn("copy_deleted_at", &now).Error; err != nil {
			log.Printf("import failure sweep: stamping row %d deleted: %v", row.ID, err)
		}
	}
}

// deleteOrphanFilesPast removes any file in the copies directory older than
// the window by its own mtime, whether or not a row points to it (security
// ruling c65222 §3) -- the backstop for a lost row or a failed transaction.
func deleteOrphanFilesPast(cutoff time.Time) {
	dir := importFailureDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("import failure sweep: reading %s: %v", dir, err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(dir, e.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("import failure sweep: deleting orphan %s: %v (retrying next pass)", path, err)
			}
		}
	}
}

// pruneOldRows deletes failure rows entirely once they are old enough that
// even the database's own backups no longer carry them (security ruling
// c65222 §4: 90 days, a margin past the 30-day backup retention).
func pruneOldRows(db *gorm.DB) {
	cutoff := time.Now().UTC().Add(-importFailureRowMaxAgeHours * time.Hour)
	if err := db.Where("created_at < ?", cutoff).Delete(&data_models.InvoicingImportFailure{}).Error; err != nil {
		log.Printf("import failure sweep: pruning rows older than 90 days: %v", err)
	}
}
