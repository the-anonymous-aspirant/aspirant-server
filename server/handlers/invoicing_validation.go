package handlers

import (
	"fmt"
	"net/http"
	"sort"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Data-integrity checks for the invoicing app (system_3 #8367-C2, spec §5).
// The issue path already refuses each of these; the report re-checks what is
// stored, so an export never ships data that drifted past a guard.

type validationItem struct {
	Code      string `json:"code"`
	InvoiceID uint   `json:"invoice_id,omitempty"`
	PaymentID uint   `json:"payment_id,omitempty"`
	Message   string `json:"message"`
}

type validationReport struct {
	OK         bool             `json:"ok"`
	Violations []validationItem `json:"violations"`
	Warnings   []validationItem `json:"warnings"`
}

// validateInvoicing runs every §5 check over the numbered (issued and
// cancelled) invoices.
func validateInvoicing(db *gorm.DB) (*validationReport, error) {
	rep := &validationReport{Violations: []validationItem{}, Warnings: []validationItem{}}
	add := func(code string, invID, payID uint, format string, args ...interface{}) {
		rep.Violations = append(rep.Violations, validationItem{code, invID, payID, fmt.Sprintf(format, args...)})
	}

	var numbered []data_models.InvoicingInvoice
	if err := db.Where("number IS NOT NULL").Order("prefix asc, number asc, id asc").Find(&numbered).Error; err != nil {
		return nil, err
	}
	today := berlinToday()

	byPrefix := map[string][]data_models.InvoicingInvoice{}
	for _, inv := range numbered {
		byPrefix[inv.Prefix] = append(byPrefix[inv.Prefix], inv)
	}
	prefixes := make([]string, 0, len(byPrefix))
	for p := range byPrefix {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		invs := byPrefix[prefix]
		for i := 1; i < len(invs); i++ {
			prev, cur := invs[i-1], invs[i]
			switch {
			case *cur.Number == *prev.Number:
				add("number_duplicate", cur.ID, 0, "invoice number %s-%03d is used twice", prefix, *cur.Number)
			case *cur.Number != *prev.Number+1:
				add("number_gap", cur.ID, 0, "numbers under %q jump from %d to %d", prefix, *prev.Number, *cur.Number)
			}
			if prev.IssueDate != nil && cur.IssueDate != nil && cur.IssueDate.Before(*prev.IssueDate) {
				add("number_order", cur.ID, 0, "invoice %s-%03d was issued on %s, before the lower number %s-%03d (%s)",
					prefix, *cur.Number, cur.IssueDate.Format(invoicingDateLayout), prefix, *prev.Number, prev.IssueDate.Format(invoicingDateLayout))
			}
		}
	}

	for _, inv := range numbered {
		if inv.IssueDate == nil {
			add("issue_date_missing", inv.ID, 0, "numbered invoice %d has no issue date", inv.ID)
			continue
		}
		issue := dateOnly(*inv.IssueDate)
		if issue.After(today) {
			add("issue_date_future", inv.ID, 0, "invoice %d is dated %s, after today", inv.ID, issue.Format(invoicingDateLayout))
		}
		end := inv.ServiceDateTo
		if end == nil {
			end = inv.ServiceDateFrom
		}
		if end != nil && dateOnly(*end).After(issue) {
			add("service_after_issue", inv.ID, 0, "invoice %d has a service date %s after its issue date %s", inv.ID, end.Format(invoicingDateLayout), issue.Format(invoicingDateLayout))
		}

		items, err := data_models.ListInvoiceLineItems(db, inv.ID)
		if err != nil {
			return nil, err
		}
		var net int64
		for _, it := range items {
			if it.LineTotalCents != data_models.LineTotalCents(it.QuantityMilli, it.UnitPriceCents) {
				add("total_mismatch_items", inv.ID, 0, "invoice %d line %d total %d does not equal quantity × price", inv.ID, it.Position, it.LineTotalCents)
			}
			net += it.LineTotalCents
		}
		if net != inv.NetCents || inv.NetCents+inv.VatCents != inv.TotalCents {
			add("total_mismatch_items", inv.ID, 0, "invoice %d stores net %d / total %d, but its line items sum to %d", inv.ID, inv.NetCents, inv.TotalCents, net)
		}

		if inv.Status != data_models.InvoiceStatusIssued {
			continue
		}
		pdf, err := data_models.GetInvoicePDF(db, inv.ID)
		if err != nil {
			return nil, err
		}
		if pdf == nil {
			add("pdf_missing", inv.ID, 0, "issued invoice %d has no stored PDF", inv.ID)
		}
		payments, err := data_models.ListInvoicePayments(db, inv.ID)
		if err != nil {
			return nil, err
		}
		var paid int64
		for _, p := range payments {
			paid += p.GrossCents
			if p.Kind != data_models.PaymentKindIncoming || p.Category != data_models.PaymentCategoryPaidWork {
				add("payment_not_paid_work", inv.ID, p.ID, "issued invoice %d covers payment %s, which is %q", inv.ID, p.TransactionID, p.Category)
			}
			other, err := data_models.IssuedInvoiceForPayment(db, p.ID, inv.ID)
			if err != nil {
				return nil, err
			}
			// Report each pair once: from the lower invoice id.
			if other != 0 && other > inv.ID {
				add("payment_multiple_invoices", inv.ID, p.ID, "payment %s is on issued invoices %d and %d", p.TransactionID, inv.ID, other)
			}
		}
		if len(payments) > 0 && paid != inv.TotalCents {
			rep.Warnings = append(rep.Warnings, validationItem{"total_mismatch_payments", inv.ID, 0,
				fmt.Sprintf("invoice %d totals %d cents, its payments %d cents", inv.ID, inv.TotalCents, paid)})
		}
	}
	rep.OK = len(rep.Violations) == 0
	return rep, nil
}

// GetInvoicingValidationHandler serves the §5 report.
func GetInvoicingValidationHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	rep, err := validateInvoicing(db)
	if err != nil {
		respondInvoicingInternal(c, nil, "validating invoices", err)
		return
	}
	c.JSON(http.StatusOK, rep)
}
