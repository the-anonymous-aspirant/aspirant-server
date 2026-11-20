package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Removing, restoring and typing in an expense category (system_3 #11579,
// design §3.161 Decisions 2 and 5; until §3.161 merges, #11566).
//
// A category ze added can be removed and restored. Removing keeps it: every
// expense that has it keeps it, and the Book, its selection, its presets and
// the CSV count it as before (§3.140 Decision 4: "A category in use is
// archived, never deleted"). It is only no longer offered for an expense
// that does not already have it. The ten starters and Private cannot be
// removed.
//
// And a category ze types into an expense's field is saved with that
// expense, both or neither: `"create_category": true` beside `category`.

const (
	expenseCategoryRemove  = "remove"
	expenseCategoryRestore = "restore"

	// The reasons the client shows, as the task words them.
	starterCategoryRefusal = "The starter categories can't be removed."
	privateCategoryRefusal = "Private can't be removed."
)

// expenseCategoryNameRefusal says why name cannot name a new category, or ""
// when it can. Private is checked separately: it is a reserved name, not a
// malformed one.
func expenseCategoryNameRefusal(name string) string {
	if name == "" || utf8.RuneCountInString(name) > maxExpenseCategoryNameRunes || strings.Contains(name, "\n") || hasControlChars(name) || hasFormatChars(name) {
		return fmt.Sprintf("must be one line of 1-%d characters", maxExpenseCategoryNameRunes)
	}
	return ""
}

// removedCategoryRefusal is the reason a removed category is not offered.
func removedCategoryRefusal(name string) string {
	return fmt.Sprintf("category %q has been removed; restore it to choose it again", name)
}

// checkExpenseCategoryChoice is the check every path that puts a category on
// an expense makes: the expense's create and edit (the row's control and
// "Set category" are edits), and the import confirms. A name the book does
// not have is refused as before. A removed one is refused too, unless it is
// the expense's current category (current; "" for a new expense), so saving
// an expense that already has it, with its category unchanged, still works.
// Returns the error code and message to answer with, both "" when allowed.
func checkExpenseCategoryChoice(tx *gorm.DB, owner uint, name, current string) (string, string, error) {
	cat, err := data_models.GetExpenseCategoryByName(tx, owner, name)
	if err != nil {
		return "", "", err
	}
	if cat == nil {
		return "invalid_field", "category is not one of your expense categories", nil
	}
	if cat.Removed() && name != current {
		return "removed_category", removedCategoryRefusal(cat.Name), nil
	}
	return "", "", nil
}

// resolveTypedExpenseCategory turns a name ze typed into an expense's
// category field into the category to save the expense with, inside the
// expense's own transaction (item B: both or neither). Ignoring case:
//   - one of zir live categories is used, under its stored name;
//   - one ze removed is restored, with its name and counts_as;
//   - otherwise a new category is made, counting as Other.
//
// A name that cannot be a category is refused with code "invalid_category"
// and a message naming the category, so the client can show it under that
// field. The caller rolls back on any refusal or error, so no category is
// left behind when the expense is refused.
func resolveTypedExpenseCategory(tx *gorm.DB, owner uint, typed string) (name, code, msg string, err error) {
	typed = strings.TrimSpace(typed)
	if refusal := expenseCategoryNameRefusal(typed); refusal != "" {
		return "", "invalid_category", "category " + refusal, nil
	}
	existing, err := data_models.FindExpenseCategoryFold(tx, owner, typed)
	if err != nil {
		return "", "", "", err
	}
	if existing != nil {
		if existing.Removed() {
			if err := setExpenseCategoryRemoved(tx, owner, existing, false); err != nil {
				return "", "", "", err
			}
		}
		return existing.Name, "", "", nil
	}
	if strings.EqualFold(typed, data_models.ExpenseCategoryPrivate) {
		// Unreachable while every book has Private (FindExpenseCategoryFold
		// finds it above); kept so a book without it cannot gain a twin.
		return "", "invalid_category", "category Private is a built-in category for expenses that never count", nil
	}
	cat := data_models.InvoicingExpenseCategory{OwnerUserID: owner, Name: typed, CountsAs: data_models.ExpenseCategoryOther}
	if err := tx.Create(&cat).Error; err != nil {
		return "", "", "", err
	}
	details := auditDetails(gin.H{"name": cat.Name, "counts_as": cat.CountsAs, "with_expense": true})
	if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionCreateManual, invoicingEntityExpenseCategory, cat.ID, details); err != nil {
		return "", "", "", err
	}
	return cat.Name, "", "", nil
}

// setExpenseCategoryRemoved removes (removed true) or restores cat, and
// writes its one audit event. The caller has checked that it changes state.
func setExpenseCategoryRemoved(tx *gorm.DB, owner uint, cat *data_models.InvoicingExpenseCategory, removed bool) error {
	action := invoicingActionRestore
	var value interface{} = gorm.Expr("NULL")
	if removed {
		action = invoicingActionRemove
		now := invoicingNow().UTC()
		value = now
		cat.RemovedAt = &now
	} else {
		cat.RemovedAt = nil
	}
	if err := tx.Model(&data_models.InvoicingExpenseCategory{}).Where("id = ? AND owner_user_id = ?", cat.ID, owner).
		Update("removed_at", value).Error; err != nil {
		return err
	}
	details := auditDetails(gin.H{"name": cat.Name, "counts_as": cat.CountsAs})
	return data_models.RecordInvoicingAuditEvent(tx, owner, action, invoicingEntityExpenseCategory, cat.ID, details)
}

// ExpenseCategoryActionHandler removes or restores one of the owner's own
// categories: POST /invoicing/expense-categories/:id/remove and
// /:id/restore. Repeating either is a no-op answered 200 with the category,
// with no second audit event. A starter or Private answers 409
// builtin_category with the reason the client shows.
func ExpenseCategoryActionHandler(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		db := c.MustGet("db").(*gorm.DB)
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil || id == 0 {
			RespondWithError(c, http.StatusBadRequest, "Invalid category id")
			return
		}
		owner := invoicingUserID(c)
		fail := func(tx *gorm.DB, what string, err error) {
			tx.Rollback()
			log.Printf("Error on expense category %d %s (%s): %v", id, action, what, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating category")
		}
		tx := db.Begin()
		if err := data_models.LockInvoicingBook(tx, owner); err != nil {
			fail(tx, "book lock", err)
			return
		}
		cat, err := data_models.GetExpenseCategory(tx, owner, uint(id))
		if err != nil {
			fail(tx, "read", err)
			return
		}
		if cat == nil {
			tx.Rollback()
			RespondWithError(c, http.StatusNotFound, "Category not found")
			return
		}
		if action == expenseCategoryRemove && cat.Builtin {
			tx.Rollback()
			msg := starterCategoryRefusal
			if cat.Name == data_models.ExpenseCategoryPrivate {
				msg = privateCategoryRefusal
			}
			respondError(c, http.StatusConflict, "builtin_category", msg)
			return
		}
		want := action == expenseCategoryRemove
		if cat.Removed() != want {
			if err := setExpenseCategoryRemoved(tx, owner, cat, want); err != nil {
				fail(tx, "write", err)
				return
			}
		}
		if err := tx.Commit().Error; err != nil {
			log.Printf("Error committing expense category %d %s: %v", id, action, err)
			RespondWithError(c, http.StatusInternalServerError, "Error updating category")
			return
		}
		c.JSON(http.StatusOK, gin.H{"category": cat})
	}
}
