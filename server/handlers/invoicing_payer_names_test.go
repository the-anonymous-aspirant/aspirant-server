package handlers

import "testing"

// Design §3.142 Decision 4 (system_3 #10539): "the same" is equality of the
// normalised names — case folded, accents stripped, ". , - '" dropped,
// spaces collapsed.
func TestNormalisePayerName(t *testing.T) {
	for in, want := range map[string]string{
		"Mara Esempio":        "mara esempio",
		"MARA ESEMPIO":        "mara esempio",
		"  mara   esempio. ":  "mara esempio",
		"Mára Ésempio":        "mara esempio",
		"Esempio-Rossi, Mara": "esempio rossi mara",
		"O'Neill":             "o neill",
		"Zoë Çelik":           "zoe celik",
		"":                    "",
		"  ":                  "",
	} {
		if got := normalisePayerName(in); got != want {
			t.Errorf("normalisePayerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameClientName(t *testing.T) {
	same := [][2]string{
		{"Mara Esempio", "MARA ESEMPIO"},
		{"Mara Esempio", "mara esempio."},
		{"Mara Esempio", "Mára  Esempio"},
		{"Galerie Nord", "galerie-nord"},
	}
	for _, p := range same {
		if !sameClientName(p[0], p[1]) {
			t.Errorf("%q and %q should be the same client", p[0], p[1])
		}
	}
	// Decision 4's "close" pairs are not "the same": they stop for her
	// choice in the form (#10539-B1) rather than linking silently.
	notSame := [][2]string{
		{"Mara Esempio", "Mara Rossi"},
		{"Mara Esempio", "Mara E."},
		{"Mara Esempio", "Esempio Mara"},
		{"Mara Esempio", "Mara Esmepio"},
		{"", ""},
		{"Mara Esempio", ""},
	}
	for _, p := range notSame {
		if sameClientName(p[0], p[1]) {
			t.Errorf("%q and %q should not be the same client", p[0], p[1])
		}
	}
}
