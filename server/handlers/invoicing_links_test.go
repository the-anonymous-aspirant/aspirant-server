package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
)

// system_3 #9971 (operator c61586): a handle or a profile link becomes one
// canonical https URL on the platform's own host; anything else is refused.
func TestSocialLinks_NormaliseOrRefuse(t *testing.T) {
	ig := map[string]string{
		"":                         "",
		"   ":                      "",
		"flora.ink":                "https://www.instagram.com/flora.ink/",
		"@Flora_Ink":               "https://www.instagram.com/flora_ink/",
		" @flora ":                 "https://www.instagram.com/flora/",
		"instagram.com/flora":      "https://www.instagram.com/flora/",
		"www.instagram.com/flora/": "https://www.instagram.com/flora/",
		"https://www.instagram.com/flora/?igsh=abc123": "https://www.instagram.com/flora/",
		"http://instagram.com/flora#top":               "https://www.instagram.com/flora/",
		"HTTPS://M.Instagram.com/Flora":                "https://www.instagram.com/flora/",
	}
	for in, want := range ig {
		if got, err := instagramPlatform.normalize(in); err != nil || got != want {
			t.Errorf("instagram %q: got %q, %v; want %q", in, got, err, want)
		}
	}
	tg := map[string]string{
		"":                          "",
		"flora_ink":                 "https://t.me/flora_ink",
		"@FloraInk":                 "https://t.me/FloraInk",
		"t.me/flora_ink":            "https://t.me/flora_ink",
		"https://t.me/flora_ink/":   "https://t.me/flora_ink",
		"https://telegram.me/flora": "https://t.me/flora",
	}
	for in, want := range tg {
		if got, err := telegramPlatform.normalize(in); err != nil || got != want {
			t.Errorf("telegram %q: got %q, %v; want %q", in, got, err, want)
		}
	}

	refusedBoth := []string{
		"javascript:alert(1)",
		"JavaScript:alert(document.cookie)",
		"data:text/html,<script>alert(1)</script>",
		"https://evil.example/flora",
		"https://instagram.com.evil.example/flora",
		"https://evil.example/instagram.com/flora",
		"//evil.example/flora",
		"ftp://instagram.com/flora",
		"https://user@instagram.com/flora",
		"https://instagram.com@evil.example/flora",
		"https://instagram.com:8443/flora",
		"https://t.me:8443/flora_ink",
		"flora ink",
		"flora/../../x",
		"<script>",
		"\"><img src=x>",
		"@",
		strings.Repeat("a", 201),
	}
	for _, in := range refusedBoth {
		if got, err := instagramPlatform.normalize(in); err == nil {
			t.Errorf("instagram %q accepted as %q", in, got)
		}
		if got, err := telegramPlatform.normalize(in); err == nil {
			t.Errorf("telegram %q accepted as %q", in, got)
		}
	}
	for _, in := range []string{
		"https://www.instagram.com/p/C0abc123/", // a post, not a profile
		"https://www.instagram.com/",            // no profile
		"https://t.me/flora",                    // the other platform's host
		"https://www.instagram.com/flora%2Fx/",  // an encoded slash
		strings.Repeat("a", 31),                 // over Instagram's 30
	} {
		if got, err := instagramPlatform.normalize(in); err == nil {
			t.Errorf("instagram %q accepted as %q", in, got)
		}
	}
	for _, in := range []string{
		"https://t.me/joinchat/AbCd", // an invite, not a profile
		"https://instagram.com/flora_ink",
		"1flora",                // must start with a letter
		"abc",                   // under 4
		"flora.ink",             // no dots
		strings.Repeat("a", 33), // over 32
	} {
		if got, err := telegramPlatform.normalize(in); err == nil {
			t.Errorf("telegram %q accepted as %q", in, got)
		}
	}
}

func errorCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, body)
	}
	return e.Error.Code
}

// The client's phone and links are stored (canonical), returned, replaced by
// a PUT and cleared by a PUT without them: the form sends the whole client.
func TestInvoicingClients_ContactFieldsRoundTrip(t *testing.T) {
	r := setupInvoicingRouter(setupInvoicingDB(t), 7)
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Anna","phone":"  +49 151 2345678 ","instagram_url":"@Anna.Ink","telegram_url":"https://t.me/anna_ink?start=x"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	c := created.Client
	if c.Phone != "+49 151 2345678" || c.InstagramURL != "https://www.instagram.com/anna.ink/" || c.TelegramURL != "https://t.me/anna_ink" {
		t.Fatalf("created: phone %q instagram %q telegram %q", c.Phone, c.InstagramURL, c.TelegramURL)
	}
	for _, key := range []string{`"phone":`, `"instagram_url":`, `"telegram_url":`} {
		if !strings.Contains(w.Body.String(), key) {
			t.Errorf("response JSON lacks %s: %s", key, w.Body.String())
		}
	}
	path := fmt.Sprintf("/invoicing/clients/%d", c.ID)
	if w := invoicingDo(r, "PUT", path, `{"name":"Anna","phone":"030 123","instagram_url":"instagram.com/anna2","telegram_url":""}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	got := listedClient(t, r, c.ID)
	if got.Phone != "030 123" || got.InstagramURL != "https://www.instagram.com/anna2/" || got.TelegramURL != "" {
		t.Errorf("after PUT: %+v", got)
	}
	if w := invoicingDo(r, "PUT", path, `{"name":"Anna"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT without them: %d %s", w.Code, w.Body.String())
	}
	if got := listedClient(t, r, c.ID); got.Phone != "" || got.InstagramURL != "" || got.TelegramURL != "" {
		t.Errorf("after a PUT without them: %+v, want all empty", got)
	}
}

func listedClient(t *testing.T, r *gin.Engine, id uint) data_models.InvoicingClient {
	t.Helper()
	w := invoicingDo(r, "GET", "/invoicing/clients", "")
	var list struct {
		Clients []data_models.InvoicingClient `json:"clients"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	for _, c := range list.Clients {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("client %d not listed: %s", id, w.Body.String())
	return data_models.InvoicingClient{}
}

// A refused phone or link is a 400 with its own code, so the editor can mark
// the field (c61870 on #10002), and writes nothing.
func TestInvoicing_ContactRefusalsHaveTheirOwnCodesAndWriteNothing(t *testing.T) {
	profile := func(extra string) string {
		return `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1,` + extra + `}`
	}
	cases := []struct {
		name, method, path, body, code string
	}{
		{"profile phone", "PUT", "/invoicing/profile", profile(`"phone":"call me"`), "invalid_phone"},
		{"profile instagram", "PUT", "/invoicing/profile", profile(`"instagram_url":"javascript:alert(1)"`), "invalid_instagram"},
		{"profile telegram", "PUT", "/invoicing/profile", profile(`"telegram_url":"https://evil.example/x"`), "invalid_telegram"},
		{"client phone", "POST", "/invoicing/clients", `{"name":"A","phone":"+49 30 <b>"}`, "invalid_phone"},
		{"client phone too long", "POST", "/invoicing/clients", `{"name":"A","phone":"` + strings.Repeat("1", 41) + `"}`, "invalid_phone"},
		{"client instagram", "POST", "/invoicing/clients", `{"name":"A","instagram_url":"https://www.instagram.com/p/abc/"}`, "invalid_instagram"},
		{"client telegram", "POST", "/invoicing/clients", `{"name":"A","telegram_url":"data:text/html,x"}`, "invalid_telegram"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupInvoicingDB(t)
			r := setupInvoicingRouter(db, 42)
			w := invoicingDo(r, tc.method, tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
			}
			if code := errorCodeOf(t, w.Body.Bytes()); code != tc.code {
				t.Errorf("error code %q, want %q", code, tc.code)
			}
			var profiles, clients int
			db.Model(&data_models.InvoicingProfile{}).Count(&profiles)
			db.Model(&data_models.InvoicingClient{}).Count(&clients)
			if profiles+clients != 0 || countAuditEvents(t, db) != 0 {
				t.Errorf("refused write stored %d profiles, %d clients, %d audit rows", profiles, clients, countAuditEvents(t, db))
			}
		})
	}
}

// updated_at moves on an edit of each new field and of the notes (operator
// c61587): the clients list's "last edited" must not miss them.
func TestInvoicing_UpdatedAtMovesOnContactAndNotesEdits(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 7)
	w := invoicingDo(r, "POST", "/invoicing/clients", `{"name":"Anna"}`)
	var created struct {
		Client data_models.InvoicingClient `json:"client"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created.Client.ID
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, field := range []string{`"notes":"booked"`, `"phone":"030 1"`, `"instagram_url":"@anna"`, `"telegram_url":"@anna_ink"`} {
		db.Model(&data_models.InvoicingClient{}).Where("id = ?", id).UpdateColumn("updated_at", old)
		if w := invoicingDo(r, "PUT", fmt.Sprintf("/invoicing/clients/%d", id), `{"name":"Anna",`+field+`}`); w.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", field, w.Code, w.Body.String())
		}
		cl, _ := data_models.GetInvoicingClient(db, id)
		if cl == nil || !cl.UpdatedAt.After(old) {
			t.Errorf("client updated_at after editing %s: %v, want later than %v", field, cl, old)
		}
	}
	for _, field := range []string{`"instagram_url":"@flora"`, `"telegram_url":"@flora_ink"`} {
		if w := invoicingDo(r, "PUT", "/invoicing/profile", `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1,`+field+`}`); w.Code != http.StatusOK {
			t.Fatalf("profile PUT %s: %d %s", field, w.Code, w.Body.String())
		}
		db.Model(&data_models.InvoicingProfile{}).Where("id = 1").UpdateColumn("updated_at", old)
		if w := invoicingDo(r, "PUT", "/invoicing/profile", `{"full_name":"F","address":"A","is_kleinunternehmer":true,"invoice_number_prefix":"2026","next_invoice_number":1}`); w.Code != http.StatusOK {
			t.Fatalf("profile PUT clearing %s: %d %s", field, w.Code, w.Body.String())
		}
		p, _ := data_models.GetInvoicingProfile(db)
		if p == nil || !p.UpdatedAt.After(old) {
			t.Errorf("profile updated_at after clearing %s: %v", field, p)
		}
	}
}

// None of the new fields print (#9971): the client's are kept out of the
// issue-time snapshot, the stored PDF and the CSV; the issuer's links stay in
// the profile snapshot (it is the profile) but the PDF prints neither.
func TestIssue_ContactFieldsReachNoInvoiceOutput(t *testing.T) {
	requirePdftotext(t)
	f := newInvoiceFixture(t)
	if w := invoicingDo(f.r, "PUT", fmt.Sprintf("/invoicing/clients/%d", f.clientID),
		`{"name":"Galerie Nord","address":"Nordweg 2\n13353 Berlin","paypal_email":"pay@galerie.test","phone":"+49 151 99887766","instagram_url":"@galerie.nord","telegram_url":"@galerie_nord"}`); w.Code != http.StatusOK {
		t.Fatalf("client PUT: %d %s", w.Code, w.Body.String())
	}
	var prof map[string]interface{}
	json.Unmarshal([]byte(validProfileBody), &prof)
	prof["instagram_url"], prof["telegram_url"] = "@flower.studio", "@flower_studio"
	body, _ := json.Marshal(prof)
	if w := invoicingDo(f.r, "PUT", "/invoicing/profile", string(body)); w.Code != http.StatusOK {
		t.Fatalf("profile PUT: %d %s", w.Code, w.Body.String())
	}

	id := f.draft(t, 0)
	code, d, resp := f.issue(id)
	if code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, resp)
	}
	clientMarks := []string{"99887766", "galerie.nord", "galerie_nord"}
	for _, m := range clientMarks {
		if strings.Contains(d.Invoice.ClientSnapshot, m) {
			t.Errorf("client snapshot carries %q: %s", m, d.Invoice.ClientSnapshot)
		}
	}
	if !strings.Contains(d.Invoice.ProfileSnapshot, "https://www.instagram.com/flower.studio/") {
		t.Errorf("profile snapshot lost the issuer's link: %s", d.Invoice.ProfileSnapshot)
	}

	w := invoicingDo(f.r, "GET", fmt.Sprintf("/invoicing/invoices/%d/pdf", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("pdf: %d", w.Code)
	}
	pdfPath := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := os.WriteFile(pdfPath, w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("pdftotext", "-q", pdfPath, "-").Output()
	if err != nil {
		t.Fatalf("pdftotext: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "Galerie Nord") {
		t.Fatalf("PDF text lacks the client itself: %q", text)
	}
	for _, m := range append(clientMarks, "instagram", "t.me", "flower.studio", "flower_studio") {
		if strings.Contains(strings.ToLower(text), strings.ToLower(m)) {
			t.Errorf("PDF prints %q", m)
		}
	}

	csv := invoicingDo(f.r, "GET", "/invoicing/export/invoices.csv", "")
	if csv.Code != http.StatusOK || !strings.Contains(csv.Body.String(), "Galerie Nord") {
		t.Fatalf("csv: %d %s", csv.Code, csv.Body.String())
	}
	for _, m := range append(clientMarks, "instagram", "t.me") {
		if strings.Contains(csv.Body.String(), m) {
			t.Errorf("CSV carries %q", m)
		}
	}

	live, _ := data_models.GetInvoicingClient(f.db, f.clientID)
	if live == nil || live.Phone != "+49 151 99887766" || live.InstagramURL == "" || live.TelegramURL == "" {
		t.Errorf("live client lost its contact fields: %+v", live)
	}
}
