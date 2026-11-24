package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// system_3 #12264: "Add client" and the client edit apply the book's one
// rule. A second client with the name of one in the book is asked about
// (409 client_exists naming it, unless confirm_second_client); a name is one
// line of at most 200 characters and an address at most 10 lines of 200,
// with no control or direction characters; a field an edit did not change
// is not checked. Every name is invented.

type clientFormFixture struct {
	db *gorm.DB
	r  *gin.Engine
}

func newClientFormFixture(t *testing.T) *clientFormFixture {
	t.Helper()
	db := setupInvoicingDB(t)
	return &clientFormFixture{db: db, r: setupInvoicingRouter(db, 5)}
}

type clientFormAnswer struct {
	Client data_models.InvoicingClient `json:"client"`
	Error  struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (f *clientFormFixture) send(t *testing.T, method, path string, body map[string]interface{}) (int, clientFormAnswer) {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := invoicingDo(f.r, method, path, string(raw))
	var a clientFormAnswer
	json.Unmarshal(w.Body.Bytes(), &a)
	return w.Code, a
}

func (f *clientFormFixture) create(t *testing.T, body map[string]interface{}) (int, clientFormAnswer) {
	return f.send(t, "POST", "/invoicing/clients", body)
}

func (f *clientFormFixture) update(t *testing.T, id uint, body map[string]interface{}) (int, clientFormAnswer) {
	return f.send(t, "PUT", fmt.Sprintf("/invoicing/clients/%d", id), body)
}

func (f *clientFormFixture) mustCreate(t *testing.T, name string) uint {
	t.Helper()
	code, a := f.create(t, map[string]interface{}{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("create %q: %d %+v", name, code, a.Error)
	}
	return a.Client.ID
}

func (f *clientFormFixture) clients(t *testing.T) int {
	t.Helper()
	var n int
	f.db.Model(&data_models.InvoicingClient{}).Count(&n)
	return n
}

func TestCreateClient_ASecondClientOfOneNameIsAskedAbout(t *testing.T) {
	f := newClientFormFixture(t)
	mara := f.mustCreate(t, "Mara Esempio")
	audits := countAuditEvents(t, f.db)

	// The same by Decision 4: case, accents and spacing aside.
	code, a := f.create(t, map[string]interface{}{"name": "  MÀRA   esempio ", "address": "Weg 1"})
	if code != http.StatusConflict || a.Error.Code != "client_exists" || a.Error.Message != "Mara Esempio is already in your client book" || a.Client.ID != mara {
		t.Fatalf("same name: %d %+v client %d; want 409 client_exists naming client %d", code, a.Error, a.Client.ID, mara)
	}
	if f.clients(t) != 1 || countAuditEvents(t, f.db) != audits {
		t.Fatalf("an asked create stored something: %d clients, %d audit rows", f.clients(t), countAuditEvents(t, f.db)-audits)
	}

	// Confirmed, it is a second client.
	code, a = f.create(t, map[string]interface{}{"name": "Mara Esempio", "confirm_second_client": true})
	if code != http.StatusCreated || a.Client.ID == mara || a.Client.Name != "Mara Esempio" {
		t.Fatalf("confirmed second: %d %+v", code, a)
	}
	// Another name is not asked about.
	f.mustCreate(t, "Mara Esempio Rossi")
	if f.clients(t) != 3 {
		t.Fatalf("clients: %d, want 3", f.clients(t))
	}
}

func TestUpdateClient_ARenameIsAskedAboutAndAKeptNameIsNot(t *testing.T) {
	f := newClientFormFixture(t)
	mara := f.mustCreate(t, "Mara Esempio")
	luca := f.mustCreate(t, "Luca Beispiel")

	code, a := f.update(t, luca, map[string]interface{}{"name": "mara esempio"})
	if code != http.StatusConflict || a.Error.Code != "client_exists" || a.Client.ID != mara {
		t.Fatalf("rename onto another: %d %+v", code, a.Error)
	}
	var stored data_models.InvoicingClient
	f.db.First(&stored, luca)
	if stored.Name != "Luca Beispiel" {
		t.Fatalf("an asked edit renamed the client to %q", stored.Name)
	}
	if code, a := f.update(t, luca, map[string]interface{}{"name": "Mara Esempio", "confirm_second_client": true}); code != http.StatusOK {
		t.Fatalf("confirmed rename: %d %+v", code, a.Error)
	}

	// Now a pair of namesakes: each stays editable with its own name, unasked.
	for _, id := range []uint{mara, luca} {
		if code, a := f.update(t, id, map[string]interface{}{"name": "Mara Esempio", "notes": "pays late"}); code != http.StatusOK {
			t.Fatalf("edit client %d of the pair: %d %+v", id, code, a.Error)
		}
	}
	// A rename that differs only by case is still the same as its namesake,
	// so it is asked about; a client with no namesake may change its own case.
	if code, a := f.update(t, mara, map[string]interface{}{"name": "MARA ESEMPIO"}); code != http.StatusConflict {
		t.Fatalf("a rename the same as its namesake: %d %+v; want 409", code, a.Error)
	}
	ada := f.mustCreate(t, "Ada Muster")
	if code, a := f.update(t, ada, map[string]interface{}{"name": "ADA MUSTER"}); code != http.StatusOK {
		t.Fatalf("its own name in capitals: %d %+v", code, a.Error)
	}
}

func TestClient_NameAndAddressBounds(t *testing.T) {
	f := newClientFormFixture(t)
	tenLines := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 200)+"\n", 10), "\n")
	for _, c := range []struct {
		what      string
		body      map[string]interface{}
		code, msg string
	}{
		{"a line break in the name", map[string]interface{}{"name": "Mara\nEsempio"}, "invalid_name", "The name is one line."},
		{"a carriage return in the name", map[string]interface{}{"name": "Mara\rEsempio"}, "invalid_name", "The name is one line."},
		{"a tab in the name", map[string]interface{}{"name": "Mara\tEsempio"}, "invalid_name", "The name has a tab, a control or a direction character. Remove it."},
		{"a NUL in the name", map[string]interface{}{"name": "Mara\x00Esempio"}, "invalid_name", "The name has a tab, a control or a direction character. Remove it."},
		{"a right-to-left override in the name", map[string]interface{}{"name": "Mara ‮oipmesE"}, "invalid_name", "The name has a tab, a control or a direction character. Remove it."},
		{"a name of 201 characters", map[string]interface{}{"name": strings.Repeat("ä", 201)}, "invalid_name", "The name is at most 200 characters."},
		{"a name of 5,000 characters", map[string]interface{}{"name": strings.Repeat("a", 5000)}, "invalid_name", "The name is at most 200 characters."},
		{"a name of punctuation only", map[string]interface{}{"name": " ... "}, "invalid_name", "A client needs a name."},
		{"an address of 11 lines", map[string]interface{}{"name": "Mara Esempio", "address": tenLines + "\nb"}, "invalid_address", "The address is at most 10 lines."},
		{"an address line of 201 characters", map[string]interface{}{"name": "Mara Esempio", "address": "Weg 1\n" + strings.Repeat("ß", 201)}, "invalid_address", "Each line of the address is at most 200 characters."},
		{"an address of 1,000,000 characters", map[string]interface{}{"name": "Mara Esempio", "address": strings.Repeat("a", 1000000)}, "invalid_address", "Each line of the address is at most 200 characters."},
		{"a tab in the address", map[string]interface{}{"name": "Mara Esempio", "address": "Weg\t1"}, "invalid_address", "The address has a tab, a control or a direction character. Remove it."},
		{"a NUL in the address", map[string]interface{}{"name": "Mara Esempio", "address": "Weg 1\x00"}, "invalid_address", "The address has a tab, a control or a direction character. Remove it."},
		{"an isolate in the address", map[string]interface{}{"name": "Mara Esempio", "address": "Weg ⁦1⁩"}, "invalid_address", "The address has a tab, a control or a direction character. Remove it."},
		{"a NUL in the notes", map[string]interface{}{"name": "Mara Esempio", "notes": "a\x00b"}, "invalid_notes", "The notes have a character that cannot be stored. Remove it."},
	} {
		code, a := f.create(t, c.body)
		if code != http.StatusBadRequest || a.Error.Code != c.code || a.Error.Message != c.msg {
			t.Errorf("%s: %d %+v; want 400 %s %q", c.what, code, a.Error, c.code, c.msg)
		}
	}
	if f.clients(t) != 0 {
		t.Fatalf("refused creates stored %d clients", f.clients(t))
	}
	// At the bounds, and with what a name may hold: a joiner, an apostrophe.
	code, a := f.create(t, map[string]interface{}{"name": strings.Repeat("ä", 200), "address": tenLines})
	if code != http.StatusCreated {
		t.Fatalf("at the bounds: %d %+v", code, a.Error)
	}
	if id := f.mustCreate(t, "Anna-Lena O'Neil क्‍ष"); id == 0 {
		t.Fatal("no id")
	}
}

// A client stored before the bounds, or a pair of namesakes, stays editable
// in the fields ze did not change; a field ze changes is checked.
func TestUpdateClient_AFieldTheEditDidNotChangeIsNotChecked(t *testing.T) {
	f := newClientFormFixture(t)
	longName := strings.Repeat("n", 300)
	twelve := strings.TrimSuffix(strings.Repeat("Zeile\n", 12), "\n")
	old := data_models.InvoicingClient{OwnerUserID: 5, Name: longName, Address: twelve}
	if err := f.db.Create(&old).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if code, a := f.update(t, old.ID, map[string]interface{}{"name": longName, "address": twelve, "notes": "kept"}); code != http.StatusOK {
		t.Fatalf("edit of the notes only: %d %+v", code, a.Error)
	}
	if code, a := f.update(t, old.ID, map[string]interface{}{"name": longName + "x", "address": twelve}); code != http.StatusBadRequest || a.Error.Code != "invalid_name" {
		t.Fatalf("a changed long name: %d %+v", code, a.Error)
	}
	if code, a := f.update(t, old.ID, map[string]interface{}{"name": longName, "address": twelve + "\nmore"}); code != http.StatusBadRequest || a.Error.Code != "invalid_address" {
		t.Fatalf("a changed long address: %d %+v", code, a.Error)
	}
	if code, a := f.update(t, old.ID, map[string]interface{}{"name": "Mara Esempio", "address": "Weg 1"}); code != http.StatusOK {
		t.Fatalf("both brought inside the bounds: %d %+v", code, a.Error)
	}
}
