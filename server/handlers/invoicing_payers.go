package handlers

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Payers from payments into the client book (system_3 #10548, under #10539;
// design §3.142 Decisions 4-6). The owner ticks which senders of her incoming
// payments become clients; one transaction adds them and links every payment
// of each ticked row. Nothing here runs on its own: every row starts unticked
// in the dialog that calls these endpoints.

const (
	maxPayersPerAdd      = 200
	maxPaymentsPerAdd    = 2000
	payerLeftOutPayPal   = "paypal_to_n26"
	payerLeftOutCategory = "other_category"
	payerLeftOutNoName   = "no_name"
)

type payerCurrencyTotal struct {
	Currency string `json:"currency"`
	Cents    int64  `json:"cents"`
}

type payerClientRef struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	PayPalEmail string `json:"paypal_email"`
}

// payerCandidate is one row of the dialog: one sender, grouped by PayPal
// email or else by Decision 4's "the same" name.
type payerCandidate struct {
	Key          string               `json:"key"`
	Payer        string               `json:"payer"`
	TidiedName   string               `json:"tidied_name"`
	PayPalEmail  string               `json:"paypal_email,omitempty"`
	PaymentIDs   []uint               `json:"payment_ids"`
	PaymentCount int                  `json:"payment_count"`
	Totals       []payerCurrencyTotal `json:"totals"`
	FirstPaidOn  string               `json:"first_paid_on"`
	LastPaidOn   string               `json:"last_paid_on"`
	// SameClient is a client already in the book that this payer is (by
	// PayPal email, or by Decision 4's "the same" name): the dialog offers it
	// rather than a new client.
	SameClient *payerClientRef `json:"same_client,omitempty"`
}

// tidyPayerName is Decision 6: trim and collapse spaces; a name written all
// in capitals or all in lower case is capitalised word by word; a name in
// mixed case is kept as written, because someone wrote it that way.
func tidyPayerName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || (s != strings.ToUpper(s) && s != strings.ToLower(s)) {
		return s
	}
	var b strings.Builder
	start := true
	for _, r := range strings.ToLower(s) {
		if start && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			start = false
			continue
		}
		b.WriteRune(r)
		start = r == ' ' || r == '-' || r == '\'' || r == '’'
	}
	return b.String()
}

// payerEmail is the grouping email of a payment: its sender's email,
// lower-cased (the PayPal import stores what PayPal printed).
func payerEmail(p data_models.InvoicingPayment) string {
	return strings.ToLower(strings.TrimSpace(p.SenderEmail))
}

// payerLeftOutReason says why an incoming, unlinked payment is not a
// candidate, or "" when it is one (Decision 5's rules, in this order).
func payerLeftOutReason(p data_models.InvoicingPayment) string {
	if isPayPalTransferIntoN26(p) {
		return payerLeftOutPayPal
	}
	if p.Category != data_models.PaymentCategoryUncategorised && p.Category != data_models.PaymentCategoryPaidWork {
		return payerLeftOutCategory
	}
	if payerEmail(p) == "" && normalisePayerName(p.SenderName) == "" {
		return payerLeftOutNoName
	}
	return ""
}

// payerKey groups a candidate payment: by PayPal email when it has one,
// otherwise by its normalised name.
func payerKey(p data_models.InvoicingPayment) string {
	if e := payerEmail(p); e != "" {
		return "email:" + e
	}
	return "name:" + normalisePayerName(p.SenderName)
}

// unlinkedIncomingPayments is the population both endpoints work from: the
// owner's incoming payments with no client yet.
func unlinkedIncomingPayments(db *gorm.DB, owner uint) ([]data_models.InvoicingPayment, error) {
	var ps []data_models.InvoicingPayment
	err := db.Where("owner_user_id = ? AND kind = ? AND client_id IS NULL", owner, data_models.PaymentKindIncoming).
		Order("paid_on asc, id asc").Find(&ps).Error
	return ps, err
}

// buildPayerCandidates groups the population into dialog rows and counts what
// it leaves out, by kind.
func buildPayerCandidates(ps []data_models.InvoicingPayment, clients []data_models.InvoicingClient) ([]payerCandidate, map[string]int) {
	leftOut := map[string]int{}
	rows := map[string]*payerCandidate{}
	var order []string
	totals := map[string]map[string]int64{}
	for _, p := range ps {
		if why := payerLeftOutReason(p); why != "" {
			leftOut[why]++
			continue
		}
		k := payerKey(p)
		row := rows[k]
		if row == nil {
			payer := strings.Join(strings.Fields(p.SenderName), " ")
			if payer == "" {
				payer = payerEmail(p)
			}
			row = &payerCandidate{Key: k, Payer: payer, TidiedName: tidyPayerName(p.SenderName), PayPalEmail: payerEmail(p)}
			rows[k] = row
			order = append(order, k)
			totals[k] = map[string]int64{}
		}
		if row.TidiedName == "" && strings.TrimSpace(p.SenderName) != "" {
			row.TidiedName = tidyPayerName(p.SenderName)
		}
		row.PaymentIDs = append(row.PaymentIDs, p.ID)
		row.PaymentCount++
		totals[k][p.Currency] += p.GrossCents
		day := p.PaidOn.Format(invoicingDateLayout)
		if row.FirstPaidOn == "" || day < row.FirstPaidOn {
			row.FirstPaidOn = day
		}
		if day > row.LastPaidOn {
			row.LastPaidOn = day
		}
	}
	out := make([]payerCandidate, 0, len(order))
	for _, k := range order {
		row := rows[k]
		for cur, cents := range totals[k] {
			row.Totals = append(row.Totals, payerCurrencyTotal{Currency: cur, Cents: cents})
		}
		sort.Slice(row.Totals, func(i, j int) bool { return row.Totals[i].Currency < row.Totals[j].Currency })
		row.SameClient = sameClientFor(row.PayPalEmail, row.Payer, clients)
		out = append(out, *row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PaymentCount != out[j].PaymentCount {
			return out[i].PaymentCount > out[j].PaymentCount
		}
		return strings.ToLower(out[i].Payer) < strings.ToLower(out[j].Payer)
	})
	return out, leftOut
}

// sameClientFor finds the book client a payer already is: one whose PayPal
// email is the payer's, else one whose name is "the same" (Decision 4).
func sameClientFor(email, name string, clients []data_models.InvoicingClient) *payerClientRef {
	if email != "" {
		for _, cl := range clients {
			if strings.EqualFold(cl.PayPalEmail, email) {
				return &payerClientRef{ID: cl.ID, Name: cl.Name, PayPalEmail: cl.PayPalEmail}
			}
		}
	}
	for _, cl := range clients {
		if sameClientName(cl.Name, name) {
			return &payerClientRef{ID: cl.ID, Name: cl.Name, PayPalEmail: cl.PayPalEmail}
		}
	}
	return nil
}

// ListPayerCandidatesHandler is GET /invoicing/payers/candidates: the rows
// of "Add payers to your client book" and the counts of what it leaves out.
func ListPayerCandidatesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)
	ps, err := unlinkedIncomingPayments(db, owner)
	if err != nil {
		log.Printf("Error listing payments for payer candidates: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing payers")
		return
	}
	clients, err := data_models.ListInvoicingClients(db, owner)
	if err != nil {
		log.Printf("Error listing clients for payer candidates: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error listing payers")
		return
	}
	rows, leftOut := buildPayerCandidates(ps, clients)
	c.JSON(http.StatusOK, gin.H{
		"payers": rows,
		"left_out": gin.H{
			payerLeftOutPayPal:   leftOut[payerLeftOutPayPal],
			payerLeftOutCategory: leftOut[payerLeftOutCategory],
			payerLeftOutNoName:   leftOut[payerLeftOutNoName],
		},
	})
}

type payersAddRow struct {
	PaymentIDs    []uint `json:"payment_ids"`
	ClientID      *uint  `json:"client_id"`
	NewClientName string `json:"new_client_name"`
}

type payersAddRequest struct {
	Payers []payersAddRow `json:"payers"`
}

// AddPayersHandler is POST /invoicing/payers: for each ticked row, link its
// payments to an existing client or to a new one, all in one transaction —
// every row is saved, or none is (Decision 5).
func AddPayersHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	owner := invoicingUserID(c)
	var req payersAddRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	bad := func(msg string) { respondError(c, http.StatusBadRequest, "invalid_field", msg) }
	if len(req.Payers) == 0 || len(req.Payers) > maxPayersPerAdd {
		bad(fmt.Sprintf("payers must list between 1 and %d rows", maxPayersPerAdd))
		return
	}
	seenPayment := map[uint]bool{}
	seenName := map[string]bool{}
	total := 0
	for i := range req.Payers {
		row := &req.Payers[i]
		row.NewClientName = strings.TrimSpace(row.NewClientName)
		if (row.ClientID == nil) == (row.NewClientName == "") {
			bad(fmt.Sprintf("payers[%d] needs exactly one of client_id and new_client_name", i))
			return
		}
		if row.NewClientName != "" {
			if !validOneLineName(row.NewClientName) || !validClientName(row.NewClientName) {
				bad(fmt.Sprintf("payers[%d].new_client_name must be one line of at most %d characters with at least one letter or digit", i, maxManualSenderRunes))
				return
			}
			n := normalisePayerName(row.NewClientName)
			if seenName[n] {
				bad(fmt.Sprintf("payers[%d].new_client_name repeats another row's new client", i))
				return
			}
			seenName[n] = true
		}
		if len(row.PaymentIDs) == 0 {
			bad(fmt.Sprintf("payers[%d].payment_ids must not be empty", i))
			return
		}
		for _, id := range row.PaymentIDs {
			if id == 0 || seenPayment[id] {
				bad(fmt.Sprintf("payers[%d].payment_ids has a zero or repeated id", i))
				return
			}
			seenPayment[id] = true
		}
		total += len(row.PaymentIDs)
	}
	if total > maxPaymentsPerAdd {
		bad(fmt.Sprintf("at most %d payments per add", maxPaymentsPerAdd))
		return
	}

	fail := func(tx *gorm.DB, what string, err error) {
		tx.Rollback()
		log.Printf("Error adding payers (%s): %v", what, err)
		RespondWithError(c, http.StatusInternalServerError, "Error adding payers")
	}
	tx := db.Begin()
	// One add at a time per book: the candidate re-read, the duplicate-name
	// check and the inserts run under the issuing lock.
	if _, err := lockIssuing(tx, owner); err != nil {
		fail(tx, "lock", err)
		return
	}
	ps, err := unlinkedIncomingPayments(tx, owner)
	if err != nil {
		fail(tx, "payments", err)
		return
	}
	// A payment is linkable only while it is still a candidate: the owner's,
	// incoming, unlinked, and not left out by rule. Anything else — linked
	// meanwhile, recategorised, deleted, or someone else's — refuses the
	// whole add, so the dialog can re-read rather than half-save.
	candidates := map[uint]data_models.InvoicingPayment{}
	for _, p := range ps {
		if payerLeftOutReason(p) == "" {
			candidates[p.ID] = p
		}
	}
	for id := range seenPayment {
		if _, ok := candidates[id]; !ok {
			tx.Rollback()
			respondError(c, http.StatusConflict, "payer_stale",
				fmt.Sprintf("payment %d is no longer waiting for a client; reload the list", id))
			return
		}
	}
	clients, err := data_models.ListInvoicingClients(tx, owner)
	if err != nil {
		fail(tx, "clients", err)
		return
	}
	emailTaken := func(email string) bool {
		for _, cl := range clients {
			if strings.EqualFold(cl.PayPalEmail, email) {
				return true
			}
		}
		return false
	}

	added, linked := 0, 0
	var addedClients []data_models.InvoicingClient
	for i, row := range req.Payers {
		// The row's PayPal email, when every payment in it carries the same one.
		email := ""
		for j, id := range row.PaymentIDs {
			e := payerEmail(candidates[id])
			if j == 0 {
				email = e
			} else if e != email {
				email = ""
				break
			}
		}
		var cl *data_models.InvoicingClient
		if row.NewClientName != "" {
			for k := range clients {
				if sameClientName(clients[k].Name, row.NewClientName) {
					tx.Rollback()
					c.JSON(http.StatusConflict, gin.H{
						"error":  ErrorDetail{Code: "client_exists", Message: fmt.Sprintf("payers[%d]: %s is already in your client book", i, clients[k].Name)},
						"client": clients[k],
					})
					return
				}
			}
			nc := data_models.InvoicingClient{OwnerUserID: owner, Name: row.NewClientName}
			// A new client from a PayPal row gets that sender's email, so later
			// PayPal imports link by themselves — unless another client already
			// answers to it, which would make the match ambiguous.
			if email != "" && !emailTaken(email) {
				nc.PayPalEmail = email
			}
			if err := tx.Create(&nc).Error; err != nil {
				fail(tx, "create client", err)
				return
			}
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionCreate, invoicingEntityClient, nc.ID, auditDetails(gin.H{"after": nc, "via": "payers"})); err != nil {
				fail(tx, "audit client", err)
				return
			}
			clients = append(clients, nc)
			addedClients = append(addedClients, nc)
			cl = &clients[len(clients)-1]
			added++
		} else {
			existing, err := data_models.GetInvoicingClient(tx, owner, *row.ClientID)
			if err != nil {
				fail(tx, "read client", err)
				return
			}
			if existing == nil {
				tx.Rollback()
				bad(fmt.Sprintf("payers[%d].client_id does not name a client", i))
				return
			}
			// An existing client gets the PayPal email only when its own is
			// empty and no other client answers to it (Decision 5).
			if email != "" && existing.PayPalEmail == "" && !emailTaken(email) {
				before := *existing
				existing.PayPalEmail = email
				if err := tx.Save(existing).Error; err != nil {
					fail(tx, "update client", err)
					return
				}
				if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionUpdate, invoicingEntityClient, existing.ID, auditDetails(gin.H{"before": before, "after": existing, "via": "payers"})); err != nil {
					fail(tx, "audit client update", err)
					return
				}
				for k := range clients {
					if clients[k].ID == existing.ID {
						clients[k].PayPalEmail = email
					}
				}
			}
			cl = existing
		}
		for _, id := range row.PaymentIDs {
			p := candidates[id]
			before := p.ClientID
			p.ClientID = &cl.ID
			if err := tx.Model(&data_models.InvoicingPayment{}).Where("id = ? AND owner_user_id = ?", p.ID, owner).
				UpdateColumn("client_id", cl.ID).Error; err != nil {
				fail(tx, "link payment", err)
				return
			}
			details := auditDetails(gin.H{
				"before": gin.H{"category": p.Category, "client_id": before},
				"after":  gin.H{"category": p.Category, "client_id": p.ClientID},
				"via":    "payers",
			})
			if err := data_models.RecordInvoicingAuditEvent(tx, owner, invoicingActionCategorise, invoicingEntityPayment, p.ID, details); err != nil {
				fail(tx, "audit payment", err)
				return
			}
			linked++
		}
	}
	if err := tx.Commit().Error; err != nil {
		log.Printf("Error committing payers add: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error adding payers")
		return
	}
	if addedClients == nil {
		addedClients = []data_models.InvoicingClient{}
	}
	c.JSON(http.StatusOK, gin.H{"clients_added": added, "payments_linked": linked, "clients": addedClients})
}
