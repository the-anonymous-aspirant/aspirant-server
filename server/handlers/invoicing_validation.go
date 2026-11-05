package handlers

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Data-integrity checks for the invoicing app (system_3 #8367-C2, spec §5).
// The issue path already refuses each of these; the report re-checks what is
// stored, so an export never ships data that drifted past a guard.
//
// Messages are read by the owner in the check screen, so they name the
// printed invoice number, amounts and dates, never database ids or cents. They
// use the screen's spellings (2026-10-05, €350.00: formatDay and formatMoney
// in the client's invoicing api.js); the German spellings belong to the PDF.

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

	label := map[uint]string{}
	for _, inv := range numbered {
		label[inv.ID] = invoiceLabel(inv)
	}
	day := screenDay

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
				add("number_duplicate", cur.ID, 0, "Invoice number %s is used twice.", label[cur.ID])
			case *cur.Number != *prev.Number+1:
				add("number_gap", cur.ID, 0, "Invoice numbers jump from %s to %s.", label[prev.ID], label[cur.ID])
			}
			if prev.IssueDate != nil && cur.IssueDate != nil && cur.IssueDate.Before(*prev.IssueDate) {
				add("number_order", cur.ID, 0, "Invoice %s (%s) is dated before %s (%s).",
					label[cur.ID], day(*cur.IssueDate), label[prev.ID], day(*prev.IssueDate))
			}
		}
	}

	for _, inv := range numbered {
		if inv.IssueDate == nil {
			add("issue_date_missing", inv.ID, 0, "Invoice %s has no issue date.", label[inv.ID])
			continue
		}
		issue := dateOnly(*inv.IssueDate)
		if issue.After(today) {
			add("issue_date_future", inv.ID, 0, "Invoice %s is dated in the future (%s).", label[inv.ID], day(issue))
		}
		end := inv.ServiceDateTo
		if end == nil {
			end = inv.ServiceDateFrom
		}
		if end != nil && dateOnly(*end).After(issue) {
			add("service_after_issue", inv.ID, 0, "Invoice %s: the service date (%s) is after the issue date (%s).", label[inv.ID], day(*end), day(issue))
		}

		items, err := data_models.ListInvoiceLineItems(db, inv.ID)
		if err != nil {
			return nil, err
		}
		var net int64
		for _, it := range items {
			if it.LineTotalCents != data_models.LineTotalCents(it.QuantityMilli, it.UnitPriceCents) {
				add("total_mismatch_items", inv.ID, 0, "Invoice %s, line %d: the total is not quantity × price.", label[inv.ID], it.Position)
			}
			net += it.LineTotalCents
		}
		if net != inv.NetCents || inv.NetCents+inv.VatCents != inv.TotalCents {
			add("total_mismatch_items", inv.ID, 0, "Invoice %s: the amounts don't add up (lines %s, net %s, total %s).", label[inv.ID],
				screenMoney(net, inv.Currency), screenMoney(inv.NetCents, inv.Currency), screenMoney(inv.TotalCents, inv.Currency))
		}

		if inv.Status != data_models.InvoiceStatusIssued {
			continue
		}
		pdf, err := data_models.GetInvoicePDF(db, inv.ID)
		if err != nil {
			return nil, err
		}
		if pdf == nil {
			add("pdf_missing", inv.ID, 0, "Invoice %s has no PDF.", label[inv.ID])
		}
		payments, err := data_models.ListInvoicePayments(db, inv.ID)
		if err != nil {
			return nil, err
		}
		var paid int64
		for _, p := range payments {
			paid += p.GrossCents
			if p.Kind != data_models.PaymentKindIncoming || p.Category != data_models.PaymentCategoryPaidWork {
				add("payment_not_paid_work", inv.ID, p.ID, "Invoice %s covers a payment that is not paid work: %s on %s (%s).",
					label[inv.ID], screenMoney(p.GrossCents, p.Currency), day(p.PaidOn), categoryLabel(p.Category))
			}
			other, err := data_models.IssuedInvoiceForPayment(db, p.ID, inv.ID)
			if err != nil {
				return nil, err
			}
			// Report each pair once: from the lower invoice id.
			if other != 0 && other > inv.ID {
				add("payment_multiple_invoices", inv.ID, p.ID, "The payment of %s on %s is on two invoices: %s and %s.",
					screenMoney(p.GrossCents, p.Currency), day(p.PaidOn), label[inv.ID], label[other])
			}
		}
		if len(payments) > 0 && paid != inv.TotalCents {
			rep.Warnings = append(rep.Warnings, validationItem{"total_mismatch_payments", inv.ID, 0,
				fmt.Sprintf("Invoice %s totals %s; its payments total %s.", label[inv.ID],
					screenMoney(inv.TotalCents, inv.Currency), screenMoney(paid, inv.Currency))})
		}
	}
	rep.OK = len(rep.Violations) == 0
	return rep, nil
}

// invoiceLabel is the number as printed on the invoice. A row from before the
// printed number was stored falls back to prefix and number.
func invoiceLabel(inv data_models.InvoicingInvoice) string {
	if inv.InvoiceNumber != nil && *inv.InvoiceNumber != "" {
		return *inv.InvoiceNumber
	}
	if inv.Number != nil {
		return fmt.Sprintf("%s-%03d", inv.Prefix, *inv.Number)
	}
	return "(no number)"
}

// screenDay is the client's formatDay: the calendar day as yyyy-mm-dd.
func screenDay(t time.Time) string { return t.Format("2006-01-02") }

// screenMoney is the client's formatMoney (Intl en-GB): €1,234.56, -€5.00,
// and for a currency other than euros the code before the amount.
func screenMoney(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	whole := fmt.Sprint(cents / 100)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	amount := fmt.Sprintf("%s.%02d", whole, cents%100)
	if currency == "" || currency == "EUR" {
		return sign + "€" + amount
	}
	return sign + currency + " " + amount
}

// paymentCategoryLabels are the client's CATEGORY_LABELS.
var paymentCategoryLabels = map[string]string{
	data_models.PaymentCategoryUncategorised: "Uncategorised",
	data_models.PaymentCategoryPaidWork:      "Paid work",
	data_models.PaymentCategoryGift:          "Gift",
	data_models.PaymentCategoryLoan:          "Loan",
	data_models.PaymentCategoryRefund:        "Refund",
	data_models.PaymentCategoryOther:         "Other",
}

func categoryLabel(c string) string {
	if l, ok := paymentCategoryLabels[c]; ok {
		return l
	}
	return c
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
