package handlers

import (
	"aspirant-online/server/data_models"
)

// clientAudit is a client as an audit row may record it: the whole client but
// its IBAN (security C2, system_3 #11551). Audit rows are kept for as long as
// the account lives, so a value written there could not be removed by
// clearing it on the client's card. Every client audit row records the client
// through this or clientAuditChange, never the bare row.
func clientAudit(cl data_models.InvoicingClient) data_models.InvoicingClient {
	cl.IBAN = ""
	return cl
}

// clientAuditChange is the before/after pair of a client update, plus whether
// the IBAN changed, which is all an audit row says about it.
func clientAuditChange(before, after data_models.InvoicingClient) map[string]interface{} {
	return map[string]interface{}{
		"before":       clientAudit(before),
		"after":        clientAudit(after),
		"iban_changed": before.IBAN != after.IBAN,
	}
}

// withVia adds the path that made the change, as the other client audit rows
// carry it.
func withVia(details map[string]interface{}, via string) map[string]interface{} {
	details["via"] = via
	return details
}
