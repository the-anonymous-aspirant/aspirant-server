package handlers

import "aspirant-online/server/data_models"

// markPrior sets a review candidate's flags from what the book already holds
// for its (source, transaction id) (system_3 #10555-B1, §3.143 Decision 6).
// already_imported stays true for a removed or deleted row, so a client that
// predates the two flags still leaves the row unticked; removed and
// deleted_earlier say which it is, so the review can show "Removed earlier:
// restore it on Transactions" and the summary can count "deleted earlier".
func markPrior(p data_models.ImportPrior, alreadyImported, removed, deletedEarlier *bool) {
	*alreadyImported = p.Seen()
	*removed = p == data_models.PriorRemoved
	*deletedEarlier = p == data_models.PriorDeleted
}

// priorCounts is embedded in every import and confirm summary. Both counts
// are subsets of the summary's duplicates, which keeps counting every row
// skipped because the book already holds it, so rows_read = imported +
// duplicates + skipped still holds and an older client's "N already
// imported" stays right.
type priorCounts struct {
	RemovedEarlier int `json:"removed_earlier"`
	DeletedEarlier int `json:"deleted_earlier"`
}

func (c *priorCounts) count(p data_models.ImportPrior) {
	switch p {
	case data_models.PriorRemoved:
		c.RemovedEarlier++
	case data_models.PriorDeleted:
		c.DeletedEarlier++
	}
}
