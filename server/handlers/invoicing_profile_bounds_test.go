package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aspirant-online/server/data_models"
)

// The profile's text bounds (system_3 #12270, the #11498 review's C3-16).

// profileBody is validProfileBody with the fields in set replaced.
func profileBody(t *testing.T, set map[string]interface{}) string {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(validProfileBody), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lines(n int, line string) string {
	return strings.TrimSuffix(strings.Repeat(line+"\n", n), "\n")
}

func TestProfileBounds_EachFieldAtAndPastItsBound(t *testing.T) {
	// "ä" is two bytes: the bound counts characters, not bytes.
	at := strings.Repeat("ä", 200)
	past := strings.Repeat("ä", 201)
	cases := []struct {
		name string
		set  map[string]interface{}
		code string // "" accepts
	}{
		{"name of 200", map[string]interface{}{"full_name": at}, ""},
		{"name of 201", map[string]interface{}{"full_name": past}, codeFullNameTooLong},
		{"name of two lines", map[string]interface{}{"full_name": "Flower\nExample"}, codeFullNameTooLong},
		{"name with a carriage return", map[string]interface{}{"full_name": "Flower\rExample"}, codeFullNameTooLong},
		{"Steuernummer of 200", map[string]interface{}{"steuernummer": at}, ""},
		{"Steuernummer of 201", map[string]interface{}{"steuernummer": past}, codeSteuernummerTooLong},
		{"Steuernummer of two lines", map[string]interface{}{"steuernummer": "12/345\n67890"}, codeSteuernummerTooLong},
		{"address of 10 lines of 200", map[string]interface{}{"address": lines(10, at)}, ""},
		{"address of 10 lines ending CRLF", map[string]interface{}{"address": strings.TrimSuffix(strings.Repeat(at+"\r\n", 10), "\r\n")}, ""},
		{"address of 11 lines", map[string]interface{}{"address": lines(11, "Beispielstr. 1")}, codeAddressTooLong},
		{"address with a line of 201", map[string]interface{}{"address": "Beispielstr. 1\n" + past}, codeAddressTooLong},
		{"address of one line of 201", map[string]interface{}{"address": past}, codeAddressTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := setupInvoicingRouter(setupInvoicingDB(t), 42)
			w := invoicingDo(r, "PUT", "/invoicing/profile", profileBody(t, tc.set))
			if tc.code == "" {
				if w.Code != http.StatusOK {
					t.Fatalf("want 200, got %d %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusBadRequest || errorCode(t, w) != tc.code {
				t.Fatalf("want 400 %s, got %d %s", tc.code, w.Code, w.Body.String())
			}
		})
	}
}

func TestProfileBounds_ARefusedSaveWritesNothing(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	events := func() int {
		var n int
		db.Model(&data_models.InvoicingAuditEvent{}).Count(&n)
		return n
	}
	before := events()
	w := invoicingDo(r, "PUT", "/invoicing/profile", profileBody(t, map[string]interface{}{
		"full_name": strings.Repeat("x", 1000000), "email": "other@example.test"}))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != codeFullNameTooLong {
		t.Fatalf("a 1,000,000-character name: %d %.200s", w.Code, w.Body.String())
	}
	p, _ := data_models.GetInvoicingProfile(db, 42)
	if p == nil || p.FullName != "Flower Example" || p.Email != "flower@example.test" {
		t.Errorf("the refused Save changed the profile: %+v", p)
	}
	if got := events(); got != before {
		t.Errorf("the refused Save wrote %d audit rows", got-before)
	}
}

// A profile stored over a bound, before the bounds, stays editable: the
// editor sends every field with every Save, and only a field the Save
// changes is checked (the #11498 review's point 1).
func TestProfileBounds_AFieldTheSaveDoesNotChangeIsNotChecked(t *testing.T) {
	db := setupInvoicingDB(t)
	r := setupInvoicingRouter(db, 42)
	if w := invoicingDo(r, "PUT", "/invoicing/profile", validProfileBody); w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	longName := strings.Repeat("n", 300)
	longAddress := lines(300, "Beispielstr. 1")
	longSteuernummer := strings.Repeat("1", 250)
	if err := db.Model(&data_models.InvoicingProfile{}).Where("owner_user_id = ?", 42).Updates(map[string]interface{}{
		"full_name": longName, "address": longAddress, "steuernummer": longSteuernummer}).Error; err != nil {
		t.Fatalf("store the old profile: %v", err)
	}
	stored := map[string]interface{}{"full_name": longName, "address": longAddress, "steuernummer": longSteuernummer}

	// Another field changes; the three over their bounds are sent back as stored.
	set := map[string]interface{}{"email": "new@example.test"}
	for k, v := range stored {
		set[k] = v
	}
	if w := invoicingDo(r, "PUT", "/invoicing/profile", profileBody(t, set)); w.Code != http.StatusOK {
		t.Fatalf("a Save of the email only: %d %.300s", w.Code, w.Body.String())
	}
	if p, _ := data_models.GetInvoicingProfile(db, 42); p == nil || p.Email != "new@example.test" || p.FullName != longName {
		t.Fatalf("the Save did not land: %+v", p)
	}

	// A field the Save changes is checked, even when the others are left over.
	for field, tc := range map[string]struct {
		value interface{}
		code  string
	}{
		"full_name":    {strings.Repeat("m", 300), codeFullNameTooLong},
		"address":      {lines(11, "Musterweg 2"), codeAddressTooLong},
		"steuernummer": {strings.Repeat("2", 250), codeSteuernummerTooLong},
	} {
		set := map[string]interface{}{}
		for k, v := range stored {
			set[k] = v
		}
		set[field] = tc.value
		w := invoicingDo(r, "PUT", "/invoicing/profile", profileBody(t, set))
		if w.Code != http.StatusBadRequest || errorCode(t, w) != tc.code {
			t.Errorf("a changed %s over its bound: %d %.200s", field, w.Code, w.Body.String())
		}
	}

	// And brought within its bound, it saves.
	set = map[string]interface{}{"full_name": "Flower Example"}
	for k, v := range stored {
		if k != "full_name" {
			set[k] = v
		}
	}
	if w := invoicingDo(r, "PUT", "/invoicing/profile", profileBody(t, set)); w.Code != http.StatusOK {
		t.Fatalf("a shortened name: %d %.300s", w.Code, w.Body.String())
	}
}
