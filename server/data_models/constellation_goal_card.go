package data_models

import "github.com/jinzhu/gorm"

// GoalCard is a Constellations victory-condition card (epic #4807). The operator
// supplied a 16-card deck (rendered PDF, task #4807) layered on top of the six
// connection types (P/D/F+/F/A/R) — it is NOT a replacement relationship
// vocabulary, so RelationshipType is untouched. Each card names a relationship
// style and the victory condition a player achieves by shaping their edges in
// the room graph.
//
// VictoryCondition is the human-readable text shown in the dictionary surface
// (#4807-B1), verbatim from the card. PredicateKey is the stable identifier the
// server-side detection engine (#4807-A2) switches on to evaluate the condition
// over the room graph — text is for humans, the key is for the machine, so
// re-wording a card never silently changes detection. MinPlayers gates
// selectability where the card itself says so ("NOT PLAYABLE WITH LESS THAN 6
// PLAYERS"); nil means playable at any room size (the count-branching cards like
// monogamy carry their 4-vs-5+ split inside the predicate, not as a floor).
type GoalCard struct {
	gorm.Model
	Code             string `json:"code" gorm:"type:varchar(40);unique;not null"`
	Name             string `json:"name" gorm:"type:varchar(60);not null"`
	VictoryCondition string `json:"victory_condition" gorm:"type:text;not null"`
	PredicateKey     string `json:"predicate_key" gorm:"type:varchar(40);not null"`
	MinPlayers       *int   `json:"min_players"`
	DisplayOrder     int    `json:"display_order" gorm:"not null;index"`
}

func intp(n int) *int { return &n }

// goalCardSeed is the source of truth for the 16 goal cards, in the operator's
// PDF order (page 1 then page 2, left column then right). VictoryCondition text
// is transcribed from the rendered card; the pairing was verified by opening and
// rasterising the PDF (task #4807 — the text-only extraction had missed the "V"
// card, so this deck has 16, not 15). Every condition is written over
// P/D/F+/F/A/R.
var goalCardSeed = []GoalCard{
	{Code: "v", Name: "V", DisplayOrder: 1, PredicateKey: "v_two_unshared",
		VictoryCondition: "Obtain two F+/dates/partners. These two players must NOT share a date/partner/F+ relationship with each other."},
	{Code: "triad", Name: "TRIAD", DisplayOrder: 2, PredicateKey: "triad_two_shared",
		VictoryCondition: "Obtain two F+/dates/partners. These two players MUST share a date/partner/F+ relationship with each other."},
	{Code: "quad", Name: "quad", DisplayOrder: 3, PredicateKey: "quad_three_no_rejection", MinPlayers: intp(6),
		VictoryCondition: "Obtain three dates/partners. They must not have rejections between each other. (Not playable with less than 6 players.)"},
	{Code: "monogamy", Name: "monogamy", DisplayOrder: 4, PredicateKey: "monogamy_exclusive",
		VictoryCondition: "4 players: one partner and one friend. 5+ players: one partner and two friends. Neither you nor your partner can have any other dates/F+/partners."},
	{Code: "single", Name: "SINGLE", DisplayOrder: 5, PredicateKey: "single_friends_rejections",
		VictoryCondition: "4 players: two friends and one rejection. 5+ players: two friends and two rejections. You must NOT have any F+/dates/partners/affairs."},
	{Code: "kitchen_table_polyamory", Name: "KITCHEN TABLE POLYAMORY", DisplayOrder: 6, PredicateKey: "kitchen_table_connected",
		VictoryCondition: "Obtain at least two dates/partners/F+. Your dates/partners/F+ must be connected but not by a rejection."},
	{Code: "unethical_non_monogamy", Name: "UNETHICAL NON-MONOGAMY", DisplayOrder: 7, PredicateKey: "unethical_non_monogamy_one_affair",
		VictoryCondition: "Obtain one F+/date/partner and one affair."},
	{Code: "hierarchical_polyamory", Name: "HIERARCHICAL POLYAMORY", DisplayOrder: 8, PredicateKey: "hierarchical_one_each", MinPlayers: intp(6),
		VictoryCondition: "Obtain one F+, one date, and one partner. You may NOT have multiple relationships of the same type, aside from friends. (Not playable with less than 6 players.)"},
	{Code: "polygamy", Name: "polygamy", DisplayOrder: 9, PredicateKey: "polygamy_two_exclusive_partners",
		VictoryCondition: "Obtain at least two partners. These partners must not have date/partner/F+ relationships with anyone but you."},
	{Code: "relationship_anarchy", Name: "RELATIONSHIP ANARCHY", DisplayOrder: 10, PredicateKey: "relationship_anarchy_three_no_rejection",
		VictoryCondition: "Obtain a relationship with at least three other players. You must not have rejections."},
	{Code: "unicorn_hunter", Name: "unicorn hunter", DisplayOrder: 11, PredicateKey: "unicorn_hunter_partner_then_date",
		VictoryCondition: "Obtain one partner FIRST, then one date. Your date and your partner must be dating as well."},
	{Code: "the_cheater", Name: "THE CHEATER", DisplayOrder: 12, PredicateKey: "cheater_one_two_affairs",
		VictoryCondition: "Obtain one F+/date/partner and two affairs."},
	{Code: "open_relationship", Name: "open relationship", DisplayOrder: 13, PredicateKey: "open_relationship_three_no_rejection",
		VictoryCondition: "Obtain three F+/dates/partners. You must not have rejections."},
	{Code: "unicorn", Name: "UNICORN", DisplayOrder: 14, PredicateKey: "unicorn_two_shared_dates",
		VictoryCondition: "Obtain two dates who share a F+/date/partner relationship."},
	{Code: "unethical_polycurious", Name: "UNETHICAL POLYCURIOUS", DisplayOrder: 15, PredicateKey: "unethical_polycurious_two_one_affair",
		VictoryCondition: "Obtain two F+/dates and one affair."},
	{Code: "the_escalator", Name: "THE ESCALATOR", DisplayOrder: 16, PredicateKey: "escalator_two_with_escalation",
		VictoryCondition: "Obtain two relationships of any kind, but one of these must escalate at some point: F -> F+ -> D -> P."},
}

// SeedGoalCards inserts the 16 goal cards idempotently, keyed on Code, and keeps
// the display fields in sync on re-run. Mirrors SeedRelationshipTypes so
// re-running AutoMigrate is safe.
func SeedGoalCards(db *gorm.DB) {
	for _, gc := range goalCardSeed {
		var existing GoalCard
		err := db.Where("code = ?", gc.Code).First(&existing).Error
		if err == gorm.ErrRecordNotFound {
			db.Create(&gc)
			continue
		}
		db.Model(&existing).Updates(map[string]interface{}{
			"name":              gc.Name,
			"victory_condition": gc.VictoryCondition,
			"predicate_key":     gc.PredicateKey,
			"min_players":       gc.MinPlayers,
			"display_order":     gc.DisplayOrder,
		})
	}
}

// GetGoalCards returns the goal-card deck ordered for display.
func GetGoalCards(db *gorm.DB) ([]GoalCard, error) {
	var cards []GoalCard
	err := db.Order("display_order ASC").Find(&cards).Error
	return cards, err
}

// GetGoalCardByID returns a single card by primary key.
func GetGoalCardByID(db *gorm.DB, id uint) (GoalCard, bool) {
	var card GoalCard
	if db.First(&card, id).Error != nil {
		return GoalCard{}, false
	}
	return card, true
}
