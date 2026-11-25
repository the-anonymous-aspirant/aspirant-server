package data_models

import "math"

// MaxInvoicingID is the largest id an invoicing row can have: the tables'
// keys are 32-bit integer columns on Postgres.
const MaxInvoicingID = math.MaxInt32

// InvoicingIDInRange reports whether id can name an invoicing row. An id
// outside the column's range names nothing, and Postgres refuses to compare
// it at all, so every lookup checks it first and answers "not found" (system_3
// #12268, the #11498 review's C3-14).
func InvoicingIDInRange(id uint) bool {
	return id >= 1 && id <= MaxInvoicingID
}

// MaxInvoiceNumber is the largest value the profile's next_invoice_number
// column holds (32-bit). Issuing draws the counter and moves it one on, so the
// counter at this value can draw no further number.
const MaxInvoiceNumber = math.MaxInt32
