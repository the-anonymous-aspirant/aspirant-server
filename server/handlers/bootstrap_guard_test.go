package handlers

import (
	"net/http"
	"sync"
	"testing"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// The bootstrap guard (system_3 #5264).
//
// `POST /bootstrap/admin` is the most dangerous public route on the service:
// unauthenticated, creates an account with the role taken from the request
// body defaulting to Admin, and since #5232 immediately loginable. Its guard
// used to count LIVE users — but `User` embeds gorm.Model, so a plain Count
// runs under `deleted_at IS NULL`, and the guard was asking "are there users
// right now" rather than "has this deployment ever been set up".
//
// Nothing reachable over HTTP drives the live count to zero today, because
// DeleteUserHandler refuses to delete the caller's own account. That is the
// point: the safety of this endpoint was an emergent property of an unrelated
// handler's rule, not a local invariant, so any future bulk delete or data
// migration would have reopened public admin creation with nothing to notice.

func newBootstrapHarness(t *testing.T) *signupHarness {
	t.Helper()
	h := newSignupHarness(t)
	h.router.POST("/bootstrap/admin", BootstrapUserHandler)
	return h
}

func bootstrapBody(username string) gin.H {
	return gin.H{
		"username": username,
		"email":    username + "@example.com",
		"password": "admin-password-1",
	}
}

// The first bootstrap on a fresh deployment works — the guard must not be so
// tight that nobody can ever set the system up.
func TestBootstrapSucceedsOnAFreshDeployment(t *testing.T) {
	h := newBootstrapHarness(t)

	if w := h.post(t, "/bootstrap/admin", bootstrapBody("admin")); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := h.user(t, "admin").Role.RoleName; got != "Admin" {
		t.Errorf("role = %q, want Admin", got)
	}
}

// The defect: soft-delete every user and the old guard's live count reaches
// zero, reopening unauthenticated Admin creation.
func TestBootstrapStaysClosedAfterEveryUserIsSoftDeleted(t *testing.T) {
	h := newBootstrapHarness(t)

	if w := h.post(t, "/bootstrap/admin", bootstrapBody("admin")); w.Code != http.StatusOK {
		t.Fatalf("first bootstrap: %d %s", w.Code, w.Body.String())
	}

	// Soft delete, exactly as DeleteUser does — the row stays, `deleted_at` is
	// set, and a scoped Count no longer sees it.
	first := h.user(t, "admin")
	if err := first.DeleteUser(h.db); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var live int64
	h.db.Model(&data_models.User{}).Count(&live)
	if live != 0 {
		t.Fatalf("live user count = %d, want 0 — this test is not reproducing the condition", live)
	}

	w := h.post(t, "/bootstrap/admin", bootstrapBody("attacker"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — public admin creation reopened when the live count hit zero", w.Code)
	}
	var attacker data_models.User
	if err := h.db.Where("username = ?", "attacker").First(&attacker).Error; err == nil {
		t.Fatal("an unauthenticated caller created an Admin after every user was soft-deleted")
	}
}

// The other half: check-and-create was not atomic, so two requests arriving on
// an empty database could both pass the count. Exactly one may win.
func TestConcurrentBootstrapsYieldExactlyOneUser(t *testing.T) {
	h := newBootstrapHarness(t)

	const racers = 6
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(n int) {
			defer wg.Done()
			// Distinct usernames, so the users table's own unique index cannot
			// be what stops the second winner — the claim has to.
			w := h.post(t, "/bootstrap/admin", bootstrapBody("admin"+string(rune('a'+n))))
			mu.Lock()
			defer mu.Unlock()
			if w.Code == http.StatusOK {
				succeeded++
			}
		}(i)
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d bootstraps succeeded, want exactly 1", succeeded)
	}
	var users int64
	h.db.Unscoped().Model(&data_models.User{}).Count(&users)
	if users != 1 {
		t.Errorf("%d users exist after the race, want 1", users)
	}
}

// The case only the Unscoped() count can catch, and the reason both gates
// exist.
//
// An existing deployment has users and NO marker row — the marker is only
// written by a bootstrap that runs after this change. If such a deployment
// soft-deletes its last user (a bulk delete, an admin script, a data
// migration), a live-scoped count reads zero and there is no marker to fall
// back on, so public admin creation reopens. Counting with Unscoped() is what
// closes it.
//
// Worth stating why this test exists at all: a negative control on
// TestBootstrapStaysClosedAfterEveryUserIsSoftDeleted passed with the scoped
// count restored, because the marker row was doing the work there. That test
// asserts the right outcome but does not isolate the property it names. This
// one does.
func TestPreMarkerDeploymentStaysClosedAfterEveryUserIsSoftDeleted(t *testing.T) {
	h := newBootstrapHarness(t)

	// A user created the ordinary way — no bootstrap, so no marker row.
	role, err := data_models.GetRoleByName(h.db, "Admin")
	if err != nil {
		t.Fatalf("GetRoleByName: %v", err)
	}
	existing := data_models.User{Username: "operator", Email: "op@example.com", RoleID: role.ID}
	if err := existing.CreateUser(h.db); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := existing.DeleteUser(h.db); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// The exact condition: zero live users, zero markers, one soft-deleted row.
	var live, markers, ever int64
	h.db.Model(&data_models.User{}).Count(&live)
	h.db.Model(&data_models.BootstrapRecord{}).Count(&markers)
	h.db.Unscoped().Model(&data_models.User{}).Count(&ever)
	if live != 0 || markers != 0 || ever != 1 {
		t.Fatalf("live=%d markers=%d ever=%d; want 0/0/1 — this test is not reproducing the condition",
			live, markers, ever)
	}

	if w := h.post(t, "/bootstrap/admin", bootstrapBody("attacker")); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a pre-marker deployment reopened public admin creation "+
			"once its users were soft-deleted", w.Code)
	}
}

// A deployment that predates the marker row — users present, no marker — must
// stay closed. This is the live system's shape at deploy time, and getting it
// wrong would reopen bootstrap on the very deployment the fix is for.
func TestExistingDeploymentWithNoMarkerStaysClosed(t *testing.T) {
	h := newBootstrapHarness(t)

	// A user created the ordinary way, with no bootstrap record anywhere.
	role, err := data_models.GetRoleByName(h.db, "Admin")
	if err != nil {
		t.Fatalf("GetRoleByName: %v", err)
	}
	existing := data_models.User{Username: "operator", Email: "op@example.com", RoleID: role.ID}
	if err := existing.CreateUser(h.db); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var markers int64
	h.db.Model(&data_models.BootstrapRecord{}).Count(&markers)
	if markers != 0 {
		t.Fatalf("marker count = %d, want 0 — this test is not reproducing a pre-marker deployment", markers)
	}

	if w := h.post(t, "/bootstrap/admin", bootstrapBody("attacker")); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — bootstrap reopened on an existing deployment", w.Code)
	}
}

// A failed create must not leave the claim behind, or one malformed request
// would lock the deployment out of ever bootstrapping.
func TestAFailedBootstrapDoesNotConsumeTheClaim(t *testing.T) {
	h := newBootstrapHarness(t)

	// No role rows at all, so resolving the role fails after the claim would
	// have been taken.
	// Unscoped: Role embeds gorm.Model too, so a plain Delete soft-deletes and
	// SeedRoles would then collide with the still-present unique index — the
	// same soft-delete-scoping shape this task is about, one table over.
	if err := h.db.Unscoped().Where("1 = 1").Delete(&data_models.Role{}).Error; err != nil {
		t.Fatalf("clearing roles: %v", err)
	}
	if w := h.post(t, "/bootstrap/admin", bootstrapBody("admin")); w.Code == http.StatusOK {
		t.Fatalf("bootstrap succeeded with no roles seeded: %s", w.Body.String())
	}

	// Put the roles back and try again: the deployment must still be
	// bootstrappable.
	if err := data_models.SeedRoles(h.db); err != nil {
		t.Fatalf("SeedRoles: %v", err)
	}
	if w := h.post(t, "/bootstrap/admin", bootstrapBody("admin")); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a failed attempt burned the one-time claim; body: %s",
			w.Code, w.Body.String())
	}
}
