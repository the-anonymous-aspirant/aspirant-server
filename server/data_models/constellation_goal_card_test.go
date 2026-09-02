package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newGoalCardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	db.AutoMigrate(&GoalCard{})
	return db
}

// The seed must insert exactly the 16 operator-supplied goal cards, ordered,
// each with victory-condition text and a predicate key. 16, not 15 — the
// text-only extraction had dropped the "V" card (task #4807).
func TestSeedGoalCardsInsertsSixteen(t *testing.T) {
	db := newGoalCardTestDB(t)
	defer db.Close()

	SeedGoalCards(db)

	cards, err := GetGoalCards(db)
	if err != nil {
		t.Fatalf("GetGoalCards: %v", err)
	}
	if len(cards) != 16 {
		t.Fatalf("got %d goal cards, want 16", len(cards))
	}

	// Order follows display_order, and the V card must be present (regression
	// guard on the extraction that missed it).
	if cards[0].Code != "v" {
		t.Errorf("cards[0].Code = %q, want %q (display_order must lead with V)", cards[0].Code, "v")
	}
	byCode := map[string]GoalCard{}
	for _, c := range cards {
		if c.VictoryCondition == "" {
			t.Errorf("card %s has empty victory_condition", c.Code)
		}
		if c.PredicateKey == "" {
			t.Errorf("card %s has empty predicate_key (the detector switches on it)", c.Code)
		}
		if c.Name == "" {
			t.Errorf("card %s has empty name", c.Code)
		}
		byCode[c.Code] = c
	}
	for _, code := range []string{"v", "triad", "quad", "monogamy", "single",
		"kitchen_table_polyamory", "unethical_non_monogamy", "hierarchical_polyamory",
		"polygamy", "relationship_anarchy", "unicorn_hunter", "the_cheater",
		"open_relationship", "unicorn", "unethical_polycurious", "the_escalator"} {
		if _, ok := byCode[code]; !ok {
			t.Errorf("goal card %q missing from seed", code)
		}
	}

	// Predicate keys must be unique — two cards sharing a key would silently
	// evaluate the same condition.
	seenKey := map[string]string{}
	for _, c := range cards {
		if prev, ok := seenKey[c.PredicateKey]; ok {
			t.Errorf("predicate_key %q shared by %s and %s", c.PredicateKey, prev, c.Code)
		}
		seenKey[c.PredicateKey] = c.Code
	}

	// Only the two cards whose text says so carry a player floor.
	if byCode["quad"].MinPlayers == nil || *byCode["quad"].MinPlayers != 6 {
		t.Errorf("quad MinPlayers = %v, want 6", byCode["quad"].MinPlayers)
	}
	if byCode["hierarchical_polyamory"].MinPlayers == nil || *byCode["hierarchical_polyamory"].MinPlayers != 6 {
		t.Errorf("hierarchical_polyamory MinPlayers = %v, want 6", byCode["hierarchical_polyamory"].MinPlayers)
	}
	if byCode["monogamy"].MinPlayers != nil {
		t.Errorf("monogamy MinPlayers = %v, want nil (its 4-vs-5+ split lives in the predicate, not a floor)", *byCode["monogamy"].MinPlayers)
	}
}

// Re-running the seed must not duplicate rows and must sync mutable fields.
func TestSeedGoalCardsIdempotent(t *testing.T) {
	db := newGoalCardTestDB(t)
	defer db.Close()

	SeedGoalCards(db)
	SeedGoalCards(db)

	var n int
	db.Model(&GoalCard{}).Count(&n)
	if n != 16 {
		t.Fatalf("after double seed got %d rows, want 16 (seed must be idempotent, keyed on code)", n)
	}
}
