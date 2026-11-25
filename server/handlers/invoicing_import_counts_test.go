package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// The PayPal import's answers read as what they are (system_3 #12271, the
// #11498 review's C3-17).

func TestPayPalImport_TooLargeAndEmptyHaveTheirOwnCodes(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 42)
	big := strings.Repeat("x", maxPayPalImportBytes+1)
	w := uploadCSV(t, r, big, "")
	if w.Code != http.StatusRequestEntityTooLarge || errorCode(t, w) != "too_large" ||
		!strings.Contains(w.Body.String(), "This file is larger than 5 MB.") {
		t.Errorf("a file over 5 MB: %d %s", w.Code, w.Body.String())
	}
	for name, content := range map[string]string{
		"no bytes":          "",
		"a line break":      "\n",
		"spaces":            "  \r\n \t",
		"a byte-order mark": "\xef\xbb\xbf",
	} {
		w := uploadCSV(t, r, content, "")
		if w.Code != http.StatusBadRequest || errorCode(t, w) != "empty_file" ||
			!strings.Contains(w.Body.String(), "This file is empty.") {
			t.Errorf("an empty file (%s): %d %s", name, w.Code, w.Body.String())
		}
	}
	// A file with only the header is not empty: it reads no rows, as before.
	if w := uploadCSV(t, r, "Date,Name,Type,Status,Currency,Gross,Transaction ID\n", "dmy"); w.Code != http.StatusOK || decodeSummary(t, w).RowsRead != 0 {
		t.Errorf("a header only: %d %s", w.Code, w.Body.String())
	}
}

// differingIDs reads the summary's "differing" from the answer as the
// client does.
func differingIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Summary struct {
			Differing []string `json:"differing"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp.Summary.Differing
}

const payPalHeader = "Date,Name,Type,Status,Currency,Gross,Transaction ID\n"

// A row whose id is in the book with another amount or date is named, and
// the stored row is kept; it still counts as already imported, so the
// summary's parts still add up to the rows read.
func TestPayPalImport_ARowThatDiffersFromTheBookIsNamed(t *testing.T) {
	db := setupPaymentsDB(t)
	r := setupPaymentsRouter(db, 42)
	first := payPalHeader +
		"14/07/2026,Mara Esempio,Payment,Completed,EUR,10.00,PPC3EUR00001\n" +
		"15/07/2026,Lia Beispiel,Payment,Completed,EUR,20.00,PPC3EUR00002\n" +
		"16/07/2026,Ada Muster,Payment,Completed,EUR,30.00,PPC3EUR00003\n" +
		"17/07/2026,Eva Probe,Payment,Completed,EUR,40.00,PPC3EUR00004\n"
	if w := uploadCSV(t, r, first, "dmy"); w.Code != http.StatusOK || decodeSummary(t, w).Imported != 4 {
		t.Fatalf("first import: %d %s", w.Code, w.Body.String())
	}
	// PPC3EUR00004 is removed: no longer in the book, so not compared.
	if err := db.Model(&data_models.InvoicingPayment{}).Where("transaction_id = ?", "PPC3EUR00004").
		UpdateColumn("removed_at", "2026-07-20 00:00:00").Error; err != nil {
		t.Fatalf("remove: %v", err)
	}

	second := payPalHeader +
		"14/07/2026,Mara Esempio,Payment,Completed,EUR,12.00,PPC3EUR00001\n" + // another amount
		"16/07/2026,Lia Beispiel,Payment,Completed,EUR,20.00,PPC3EUR00002\n" + // another day
		"16/07/2026,Ada Muster,Payment,Completed,EUR,30.00,PPC3EUR00003\n" + // the same
		"17/07/2026,Eva Probe,Payment,Completed,EUR,41.00,PPC3EUR00004\n" + // removed
		"18/07/2026,Neu Kundin,Payment,Completed,EUR,5.00,PPC3EUR00005\n" // new
	w := uploadCSV(t, r, second, "dmy")
	if w.Code != http.StatusOK {
		t.Fatalf("second import: %d %s", w.Code, w.Body.String())
	}
	s := decodeSummary(t, w)
	if got := differingIDs(t, w); strings.Join(got, ",") != "PPC3EUR00001,PPC3EUR00002" {
		t.Errorf("differing %v, want PPC3EUR00001,PPC3EUR00002", got)
	}
	if s.RowsRead != 5 || s.Imported != 1 || s.Duplicates != 4 || s.RowsRead != s.Imported+s.Duplicates+s.Skipped {
		t.Errorf("counts do not add up: %+v", s)
	}
	var kept data_models.InvoicingPayment
	db.Where("transaction_id = ?", "PPC3EUR00001").First(&kept)
	if kept.GrossCents != 1000 {
		t.Errorf("the stored row changed: %+v", kept)
	}
	// A re-upload of the same file names nothing.
	if w := uploadCSV(t, r, first, "dmy"); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"differing"`) {
		t.Errorf("the first file again: %s", w.Body.String())
	}
}
