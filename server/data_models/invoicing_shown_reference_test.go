package data_models

import (
	"encoding/json"
	"testing"
)

// system_3 #12257: a view shows the provider's id, the owner's own reference
// for a hand entry, or nothing when the server derived the id for an N26 row.
// The JSON carries that as `shown_reference`, beside the stored fields.
func TestShownReference_PaymentsAndExpenses(t *testing.T) {
	cases := []struct {
		name, source, entry, txn, reference, want string
	}{
		{"N26 CSV row", "n26", PaymentEntryImport, "csv-f00dfeedcafe0123456789ab", "", ""},
		{"N26 statement row", "n26", PaymentEntryImport, "stmt-0123456789abcdef01234567", "", ""},
		{"N26 transfer confirmation", "n26", PaymentEntryImport, "a1b2c3d4-0000-4000-8000-000000000001", "", "a1b2c3d4-0000-4000-8000-000000000001"},
		{"PayPal import", "paypal", PaymentEntryImport, "1AA11111AA1111111", "", "1AA11111AA1111111"},
		{"PayPal id that happens to start csv-", "paypal", PaymentEntryImport, "csv-not-derived", "", "csv-not-derived"},
		{"hand entry with a reference", "manual", PaymentEntryManual, "M-0000000000000001", "RE-2026-17", "RE-2026-17"},
		{"hand entry without one", "manual", PaymentEntryManual, "M-0000000000000002", "", ""},
	}
	for _, c := range cases {
		p := InvoicingPayment{Source: c.source, Entry: c.entry, TransactionID: c.txn, Reference: c.reference}
		if got := p.ShownReference(); got != c.want {
			t.Errorf("payment, %s: ShownReference %q, want %q", c.name, got, c.want)
		}
		if got := p.PrintedReference(); got != c.want {
			t.Errorf("payment, %s: PrintedReference %q, want %q", c.name, got, c.want)
		}
		var pj map[string]interface{}
		raw, err := json.Marshal(p)
		if err != nil || json.Unmarshal(raw, &pj) != nil {
			t.Fatalf("payment, %s: marshal: %v", c.name, err)
		}
		if pj["shown_reference"] != c.want || pj["transaction_id"] != c.txn || pj["reference"] != c.reference {
			t.Errorf("payment, %s: JSON %s", c.name, raw)
		}

		// An imported expense keeps its payee in Reference; it is never shown
		// as the reference.
		ref := c.reference
		if c.entry == PaymentEntryImport {
			ref = "Invented Payee GmbH"
		}
		e := InvoicingExpense{Source: c.source, Entry: c.entry, TransactionID: c.txn, Reference: ref}
		if got := e.ShownReference(); got != c.want {
			t.Errorf("expense, %s: ShownReference %q, want %q", c.name, got, c.want)
		}
		var ej map[string]interface{}
		raw, err = json.Marshal(&e)
		if err != nil || json.Unmarshal(raw, &ej) != nil {
			t.Fatalf("expense, %s: marshal: %v", c.name, err)
		}
		if ej["shown_reference"] != c.want || ej["transaction_id"] != c.txn {
			t.Errorf("expense, %s: JSON %s", c.name, raw)
		}
	}
}

func TestMethodLabel_N26IsBankTransferAndASourceNameNeverPrints(t *testing.T) {
	for source, want := range map[string]string{"n26": "Banküberweisung", "paypal": "PayPal", "stripe": "Stripe", "invented-bank": ""} {
		p := InvoicingPayment{Source: source, Entry: PaymentEntryImport}
		if got := p.MethodLabel(); got != want {
			t.Errorf("%s: MethodLabel %q, want %q", source, got, want)
		}
	}
}
