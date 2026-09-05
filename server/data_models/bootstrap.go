package data_models

import (
	"errors"
	"time"

	"github.com/jinzhu/gorm"
)

// BootstrapRecord is the permanent, one-row mark that this deployment has been
// bootstrapped (system_3 #5264).
//
// It replaces a guard that asked the wrong question. BootstrapUserHandler used
// to refuse when any user existed — but `User` embeds gorm.Model, so it
// soft-deletes, and a plain Count runs under gorm's `deleted_at IS NULL` scope.
// The guard therefore measured "are there live users right now", not "has this
// deployment ever been set up". If the live count ever reached zero, the single
// most dangerous public route on the service — unauthenticated, creates an
// Admin, and since #5232 marks it immediately loginable — silently reopened.
//
// Nothing reachable over HTTP can drive the live count to zero today, because
// DeleteUserHandler refuses to delete the caller's own account. That is exactly
// why this is worth fixing: the safety of the most consequential endpoint on
// the service was an emergent property of an unrelated handler's rule rather
// than anything local, so any future bulk delete, admin script or data
// migration would reopen it with nothing to notice.
//
// A row rather than a count also settles the check-and-create race. The old
// guard read the count and created the user in two steps, so two requests
// arriving on an empty database could both pass — an attacker racing the
// deployer during the deploy-to-first-admin window. This row's primary key is
// fixed, so the second insert violates it and exactly one bootstrap wins,
// whatever the interleaving.
type BootstrapRecord struct {
	// ID is pinned to bootstrapRecordID: at most one row can ever exist, and
	// that is the whole mechanism.
	ID        uint `gorm:"primary_key"`
	CreatedAt time.Time
	// Username of the account the successful bootstrap created, for the record.
	Username string
}

// TableName pins the table name rather than relying on the pluraliser, since
// this is a migration surface.
func (BootstrapRecord) TableName() string { return "bootstrap_records" }

// bootstrapRecordID is the fixed primary key of the single marker row.
const bootstrapRecordID = 1

// ErrAlreadyBootstrapped reports that this deployment has been set up before.
var ErrAlreadyBootstrapped = errors.New("bootstrap: this deployment has already been bootstrapped")

// ClaimBootstrap takes the one-time right to create the first account, inside
// tx, and returns ErrAlreadyBootstrapped if it has been taken.
//
// Two independent gates, and both are wanted:
//
//  1. Any user that has EVER existed — counted with Unscoped(), so soft-deleted
//     rows still count. This is what covers a deployment that predates the
//     marker row, including the live one: it has users and no marker, and must
//     stay closed.
//  2. The marker row itself, inserted at a fixed primary key. This is what
//     makes the claim permanent and atomic — a second concurrent insert
//     violates the key, so exactly one caller wins however the two interleave.
//
// The caller runs this inside the same transaction as the user create, so a
// failure anywhere rolls the claim back and the next attempt is clean.
func ClaimBootstrap(tx *gorm.DB, username string) error {
	var everExisted int64
	if err := tx.Unscoped().Model(&User{}).Count(&everExisted).Error; err != nil {
		return err
	}
	if everExisted > 0 {
		return ErrAlreadyBootstrapped
	}

	var existing BootstrapRecord
	err := tx.Where("id = ?", bootstrapRecordID).First(&existing).Error
	if err == nil {
		return ErrAlreadyBootstrapped
	}
	if !gorm.IsRecordNotFoundError(err) {
		return err
	}

	// The insert is the atomic part: on a race the loser hits the primary-key
	// violation rather than a second successful bootstrap.
	if err := tx.Create(&BootstrapRecord{
		ID:        bootstrapRecordID,
		CreatedAt: time.Now(),
		Username:  username,
	}).Error; err != nil {
		return ErrAlreadyBootstrapped
	}
	return nil
}
