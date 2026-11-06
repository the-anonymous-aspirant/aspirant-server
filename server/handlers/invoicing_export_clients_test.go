package handlers

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #10012 (from #9978, operator c61628): her client list as a CSV.

func clientsCSVRouter(db *gorm.DB) *gin.Engine {
	r := setupInvoicingRouter(db, 7)
	r.GET("/invoicing/export/clients.csv", ExportClientsCSVHandler)
	return r
}

func readClientsCSV(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatalf("no UTF-8 BOM: %q", body[:min(len(body), 20)])
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\ufeff")))
	r.Comma = ';'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return rows
}

func seedClient(t *testing.T, db *gorm.DB, cl data_models.InvoicingClient, created, updated time.Time) {
	t.Helper()
	if err := db.Create(&cl).Error; err != nil {
		t.Fatalf("seed %s: %v", cl.Name, err)
	}
	// gorm stamps both on create; set the moments the test asserts.
	if err := db.Model(&data_models.InvoicingClient{}).Where("id = ?", cl.ID).UpdateColumns(map[string]interface{}{"created_at": created, "updated_at": updated}).Error; err != nil {
		t.Fatalf("stamp %s: %v", cl.Name, err)
	}
}

func TestExportClientsCSV_HeaderRowsAndOrder(t *testing.T) {
	db := setupInvoicingDB(t)
	r := clientsCSVRouter(db)
	winter := time.Date(2026, 11, 5, 14, 7, 0, 0, time.UTC) // 15:07 in Berlin (CET)
	summer := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)   // 10:00 in Berlin (CEST)
	seedClient(t, db, data_models.InvoicingClient{
		Name: "Zora Beispiel", Address: "Weg 1\n10115 Berlin", Email: "zora@example.test",
		Phone: "+49 30 1234567", InstagramURL: "https://www.instagram.com/zora/", TelegramURL: "https://t.me/zora_b",
		Notes: "Private: prefers mornings.",
	}, summer, winter)
	seedClient(t, db, data_models.InvoicingClient{Name: "Anna Example", PayPalEmail: "anna@pay.test"}, winter, winter)

	w := invoicingDo(r, "GET", "/invoicing/export/clients.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="Kunden.csv"` {
		t.Errorf("disposition %q", cd)
	}
	rows := readClientsCSV(t, w.Body.String())
	want := [][]string{
		{"Name", "Adresse", "E-Mail", "PayPal-E-Mail", "Telefon", "Instagram", "Telegram", "Angelegt am", "Geändert am"},
		// Name order; Berlin time; address lines joined; a "+" phone is kept as text.
		{"Anna Example", "", "", "anna@pay.test", "", "", "", "2026-11-05 15:07", "2026-11-05 15:07"},
		{"Zora Beispiel", "Weg 1, 10115 Berlin", "zora@example.test", "", "'+49 30 1234567", "https://www.instagram.com/zora/", "https://t.me/zora_b", "2026-07-01 10:00", "2026-11-05 15:07"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows: got %d, want %d: %q", len(rows), len(want), rows)
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d:\n got %q\nwant %q", i, rows[i], want[i])
		}
	}
}

// #9978 decision 1: her notes are never exported, even when set.
func TestExportClientsCSV_NotesAreNeverExported(t *testing.T) {
	db := setupInvoicingDB(t)
	r := clientsCSVRouter(db)
	now := time.Now().UTC()
	seedClient(t, db, data_models.InvoicingClient{Name: "Notiz Beispiel", Notes: "Owes a touch-up; call before May."}, now, now)
	body := invoicingDo(r, "GET", "/invoicing/export/clients.csv", "").Body.String()
	if strings.Contains(body, "touch-up") || strings.Contains(strings.ToLower(body), "notiz;") || strings.Contains(body, "Notizen") {
		t.Errorf("notes reached the export: %q", body)
	}
	if !strings.Contains(body, "Notiz Beispiel") {
		t.Fatalf("the client itself is missing: %q", body)
	}
}

// @security c51455: a cell that starts like a formula is written as text.
func TestExportClientsCSV_FormulaLookingValuesAreNeutralised(t *testing.T) {
	db := setupInvoicingDB(t)
	r := clientsCSVRouter(db)
	now := time.Now().UTC()
	seedClient(t, db, data_models.InvoicingClient{Name: `=HYPERLINK("http://evil.example","x")`, Address: "@SUM(1)", Email: "-1+1@example.test"}, now, now)
	rows := readClientsCSV(t, invoicingDo(r, "GET", "/invoicing/export/clients.csv", "").Body.String())
	got := rows[1]
	for i, cell := range got[:4] {
		if cell != "" && strings.ContainsAny(cell[:1], "=+-@") {
			t.Errorf("column %q starts with a formula character: %q", clientsCSVHeader[i], cell)
		}
	}
	if got[0] != `'=HYPERLINK("http://evil.example","x")` {
		t.Errorf("name cell: %q", got[0])
	}
}

func TestExportClientsCSV_AuditsTheExportWithItsCount(t *testing.T) {
	db := setupInvoicingDB(t)
	r := clientsCSVRouter(db)
	now := time.Now().UTC()
	seedClient(t, db, data_models.InvoicingClient{Name: "A"}, now, now)
	seedClient(t, db, data_models.InvoicingClient{Name: "B"}, now, now)
	before := countAuditEvents(t, db)
	if w := invoicingDo(r, "GET", "/invoicing/export/clients.csv", ""); w.Code != http.StatusOK {
		t.Fatalf("export: %d", w.Code)
	}
	if n := countAuditEvents(t, db); n != before+1 {
		t.Fatalf("audit rows: %d, want %d", n, before+1)
	}
	var ev data_models.InvoicingAuditEvent
	db.Order("id desc").First(&ev)
	var details map[string]interface{}
	if err := json.Unmarshal([]byte(ev.Details), &details); err != nil {
		t.Fatalf("details: %v (%q)", err, ev.Details)
	}
	if ev.Action != invoicingActionExport || ev.EntityType != invoicingEntityExport || details["kind"] != "clients_csv" || details["clients"] != float64(2) {
		t.Errorf("audit row: action %q entity %q details %v", ev.Action, ev.EntityType, details)
	}
}

func TestExportClientsCSV_NoClientsIsTheHeaderAlone(t *testing.T) {
	db := setupInvoicingDB(t)
	rows := readClientsCSV(t, invoicingDo(clientsCSVRouter(db), "GET", "/invoicing/export/clients.csv", "").Body.String())
	if len(rows) != 1 || rows[0][0] != "Name" {
		t.Errorf("empty export: %q", rows)
	}
}
