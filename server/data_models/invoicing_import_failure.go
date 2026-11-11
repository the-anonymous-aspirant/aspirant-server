package data_models

import "time"

// InvoicingImportFailure is a durable record of one failed or empty invoice
// import upload (system_3 #10536, operator ask c65185 on #9656; security
// ruling c65222). Written whenever an N26 or PayPal import file fails to
// parse, or parses to zero usable rows.
//
// ErrorMessage is always one of a closed set of fixed strings per
// ErrorCode -- never the parser's own error text, which can quote values
// from inside the document (security ruling c65222 §4). Filename is kept
// (it is the uploader's own choice, not document content), but these rows
// are pruned after 90 days because the database's own backups keep a dump
// for up to 30 days past any deletion.
//
// CopyPath names a file under the dedicated copies directory (never a
// public asset path; see docs/OPERATIONS.md). It is empty when no copy was
// kept: the file exceeded the import's own size cap, or the copies
// directory was at its own cap when this row was written. CopyDeletedAt is
// set by the sweep once the retention window has passed, or immediately if
// no copy was ever kept.
type InvoicingImportFailure struct {
	ID             uint       `gorm:"primary_key"`
	OwnerUserID    uint       `gorm:"index"`
	CreatedAt      time.Time  `gorm:"index"`
	ImportKind     string     // "n26" or "paypal"
	Filename       string
	SizeBytes      int64
	SHA256         string
	ErrorCode      string
	ErrorMessage   string
	DocumentKind   string // best-effort; empty when never detected
	CandidateCount int
	CopyPath       string `gorm:"column:copy_path"`
	CopyDeletedAt  *time.Time
}

func (InvoicingImportFailure) TableName() string { return "invoicing_import_failures" }
