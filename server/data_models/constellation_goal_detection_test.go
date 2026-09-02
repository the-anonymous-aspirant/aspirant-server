package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

type edge struct {
	a, b uint
	code string
}

// graph builds a roomGraph directly so predicates are tested against precise
// hand-laid connection sets without DB round-trips.
func graph(playerCount int, players []uint, edges ...edge) *roomGraph {
	g := &roomGraph{edges: map[[2]uint]string{}, players: players, playerCount: playerCount}
	for _, e := range edges {
		g.edges[pairKey(e.a, e.b)] = e.code
	}
	return g
}

var p6 = []uint{1, 2, 3, 4, 5, 6}

func TestPredV(t *testing.T) {
	// Two romantic partners (2,3) with no romantic edge between them → V.
	if !predV(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "R"}), 1) {
		t.Error("V: two romantic partners not sharing a romantic edge should satisfy V")
	}
	// The near-miss: 2,3 DO share a romantic edge → that is TRIAD, not V.
	if predV(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "P"}), 1) {
		t.Error("V: partners sharing a romantic edge must NOT satisfy V (that is TRIAD)")
	}
}

func TestPredTriad(t *testing.T) {
	if !predTriad(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "F+"}), 1) {
		t.Error("TRIAD: two romantic partners sharing a romantic edge should satisfy TRIAD")
	}
	if predTriad(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, ""}), 1) {
		t.Error("TRIAD: partners not sharing a romantic edge must NOT satisfy TRIAD (that is V)")
	}
}

func TestPredUnicorn(t *testing.T) {
	// Two DATES (not just any romantic) who share a romantic edge.
	if !predUnicorn(graph(4, p6, edge{1, 2, "D"}, edge{1, 3, "D"}, edge{2, 3, "P"}), 1) {
		t.Error("UNICORN: two dates sharing a romantic edge should satisfy")
	}
	// Near-miss: one is a partner, not a date → not "two dates".
	if predUnicorn(graph(4, p6, edge{1, 2, "D"}, edge{1, 3, "P"}, edge{2, 3, "P"}), 1) {
		t.Error("UNICORN: needs two DATES specifically, not any romantic")
	}
	// Near-miss: two dates but they do not share a romantic edge.
	if predUnicorn(graph(4, p6, edge{1, 2, "D"}, edge{1, 3, "D"}, edge{2, 3, "F"}), 1) {
		t.Error("UNICORN: the two dates must share a F+/date/partner edge")
	}
}

func TestPredQuad(t *testing.T) {
	// 6 players, three dates/partners with no rejection among them.
	if !predQuad(graph(6, p6, edge{1, 2, "D"}, edge{1, 3, "P"}, edge{1, 4, "D"}), 1) {
		t.Error("quad: three D/P with no rejection among them, 6 players, should satisfy")
	}
	// Not playable below 6 players.
	if predQuad(graph(4, p6, edge{1, 2, "D"}, edge{1, 3, "P"}, edge{1, 4, "D"}), 1) {
		t.Error("quad: must be false below 6 players")
	}
	// A rejection between two of the three breaks it.
	if predQuad(graph(6, p6, edge{1, 2, "D"}, edge{1, 3, "P"}, edge{1, 4, "D"}, edge{2, 3, "R"}), 1) {
		t.Error("quad: a rejection between the partners must break it")
	}
}

func TestPredMonogamy(t *testing.T) {
	// 4 players: one partner + one friend, partner exclusive → satisfied.
	if !predMonogamy(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "F"}), 1) {
		t.Error("monogamy(4p): one partner + one friend, exclusive, should satisfy")
	}
	// 5 players needs TWO friends: one friend is not enough.
	if predMonogamy(graph(5, p6, edge{1, 2, "P"}, edge{1, 3, "F"}), 1) {
		t.Error("monogamy(5p): one friend must be insufficient (needs two)")
	}
	if !predMonogamy(graph(5, p6, edge{1, 2, "P"}, edge{1, 3, "F"}, edge{1, 4, "F"}), 1) {
		t.Error("monogamy(5p): one partner + two friends, exclusive, should satisfy")
	}
	// A stray date breaks exclusivity for me.
	if predMonogamy(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "F"}, edge{1, 4, "D"}), 1) {
		t.Error("monogamy: my own extra date must break it")
	}
	// The partner having a romantic edge with someone else breaks it.
	if predMonogamy(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "F"}, edge{2, 4, "D"}), 1) {
		t.Error("monogamy: the partner's date with a third player must break it")
	}
}

func TestPredSingle(t *testing.T) {
	// 4 players: two friends + one rejection, no romantic/affair.
	if !predSingle(graph(4, p6, edge{1, 2, "F"}, edge{1, 3, "F"}, edge{1, 4, "R"}), 1) {
		t.Error("SINGLE(4p): two friends + one rejection should satisfy")
	}
	// 5 players needs two rejections.
	if predSingle(graph(5, p6, edge{1, 2, "F"}, edge{1, 3, "F"}, edge{1, 4, "R"}), 1) {
		t.Error("SINGLE(5p): one rejection must be insufficient (needs two)")
	}
	// Any F+/date/partner/affair breaks it.
	if predSingle(graph(4, p6, edge{1, 2, "F"}, edge{1, 3, "F"}, edge{1, 4, "R"}, edge{1, 5, "F+"}), 1) {
		t.Error("SINGLE: a stray F+ must break it")
	}
}

func TestPredKitchenTable(t *testing.T) {
	// Two romantic partners connected to each other, no rejection.
	if !predKitchenTable(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "F+"}), 1) {
		t.Error("kitchen-table: partners connected via romantic edge should satisfy")
	}
	// Partners not connected to each other → not "connected".
	if predKitchenTable(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, ""}), 1) {
		t.Error("kitchen-table: unconnected partners must fail")
	}
	// A rejection among the partners breaks it even if otherwise connected.
	if predKitchenTable(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "R"}), 1) {
		t.Error("kitchen-table: a rejection among partners must break it")
	}
}

func TestPredUnethicalNonMonogamy(t *testing.T) {
	if !predUnethicalNonMonogamy(graph(4, p6, edge{1, 2, "D"}, edge{1, 3, "A"}), 1) {
		t.Error("unethical-non-monogamy: one romantic + one affair should satisfy")
	}
	if predUnethicalNonMonogamy(graph(4, p6, edge{1, 2, "D"}), 1) {
		t.Error("unethical-non-monogamy: needs an affair")
	}
}

func TestPredHierarchical(t *testing.T) {
	// 6 players: exactly one P, one D, one F+.
	if !predHierarchical(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{1, 4, "F+"}), 1) {
		t.Error("hierarchical(6p): one each of P/D/F+ should satisfy")
	}
	// Below 6 players → not playable.
	if predHierarchical(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{1, 4, "F+"}), 1) {
		t.Error("hierarchical: must be false below 6 players")
	}
	// A duplicate partner (two P) breaks "no multiple of the same type".
	if predHierarchical(graph(6, p6, edge{1, 2, "P"}, edge{1, 5, "P"}, edge{1, 3, "D"}, edge{1, 4, "F+"}), 1) {
		t.Error("hierarchical: two partners must break it")
	}
}

func TestPredPolygamy(t *testing.T) {
	// Two partners, neither romantically involved with a third → satisfied.
	if !predPolygamy(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "P"}), 1) {
		t.Error("polygamy: two exclusive partners should satisfy")
	}
	// Only one partner is not enough.
	if predPolygamy(graph(6, p6, edge{1, 2, "P"}), 1) {
		t.Error("polygamy: one partner is insufficient")
	}
	// A partner dating a third player breaks exclusivity.
	if predPolygamy(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "P"}, edge{2, 4, "D"}), 1) {
		t.Error("polygamy: a partner's date with a third player must break it")
	}
}

func TestPredRelationshipAnarchy(t *testing.T) {
	// Three relationships (any of P/D/F+/F), no rejection.
	if !predRelationshipAnarchy(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "F"}, edge{1, 4, "F+"}), 1) {
		t.Error("anarchy: three relationships and no rejection should satisfy")
	}
	// A rejection on me breaks it.
	if predRelationshipAnarchy(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "F"}, edge{1, 4, "F+"}, edge{1, 5, "R"}), 1) {
		t.Error("anarchy: any rejection on me must break it")
	}
	// Affairs do not count as "a relationship" toward the three.
	if predRelationshipAnarchy(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "F"}, edge{1, 4, "A"}), 1) {
		t.Error("anarchy: an affair must not count toward the three relationships")
	}
}

func TestPredCheater(t *testing.T) {
	if !predCheater(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "A"}, edge{1, 4, "A"}), 1) {
		t.Error("cheater: one romantic + two affairs should satisfy")
	}
	// One affair is CHEATER's near-miss (that is unethical-non-monogamy).
	if predCheater(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "A"}), 1) {
		t.Error("cheater: needs TWO affairs, one is not enough")
	}
}

func TestPredOpenRelationship(t *testing.T) {
	if !predOpenRelationship(graph(6, p6, edge{1, 2, "F+"}, edge{1, 3, "D"}, edge{1, 4, "P"}), 1) {
		t.Error("open: three F+/dates/partners, no rejection should satisfy")
	}
	// A rejection breaks it.
	if predOpenRelationship(graph(6, p6, edge{1, 2, "F+"}, edge{1, 3, "D"}, edge{1, 4, "P"}, edge{1, 5, "R"}), 1) {
		t.Error("open: a rejection on me must break it")
	}
	// Two is not enough.
	if predOpenRelationship(graph(6, p6, edge{1, 2, "F+"}, edge{1, 3, "D"}), 1) {
		t.Error("open: needs three")
	}
}

func TestPredPolycurious(t *testing.T) {
	// Two F+/dates + one affair.
	if !predPolycurious(graph(6, p6, edge{1, 2, "F+"}, edge{1, 3, "D"}, edge{1, 4, "A"}), 1) {
		t.Error("polycurious: two romantic + one affair should satisfy")
	}
	// Q5 override: partners DO count toward the two.
	if !predPolycurious(graph(6, p6, edge{1, 2, "P"}, edge{1, 3, "P"}, edge{1, 4, "A"}), 1) {
		t.Error("polycurious: partners must count toward the two (operator override Q5)")
	}
	// One romantic is not enough — distinguishes it from the-cheater's affair count.
	if predPolycurious(graph(6, p6, edge{1, 2, "P"}, edge{1, 4, "A"}), 1) {
		t.Error("polycurious: needs TWO romantic")
	}
}

// The two history-dependent cards are wired into the engine by #4829-A3. They
// share the snapshot predicate signature but read g.events; their dedicated
// behaviour is covered in constellation_relationship_event_test.go.
func TestHistoryPredicatesRegisteredByA3(t *testing.T) {
	for _, key := range []string{"unicorn_hunter_partner_then_date", "escalator_two_with_escalation"} {
		if _, present := goalHistoryPredicates[key]; !present {
			t.Errorf("history predicate %q must be registered in the history engine (#4829-A3)", key)
		}
		if _, present := goalPredicates[key]; present {
			t.Errorf("history predicate %q must NOT be in the snapshot map (it reads the event log)", key)
		}
	}
}

// Integration: EvaluateGoalAchieved reads the FULL room graph, so a goal is
// achieved off edges the selecting player cannot see in their own /state.
func TestEvaluateGoalAchieved_ReadsFullGraph(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &GoalCard{}, &PlayerGoal{})
	SeedRelationshipTypes(db)
	SeedGoalCards(db)
	room, _, _ := CreateRoom(db, 1, 4)
	JoinRoom(db, 2, room.Code)
	JoinRoom(db, 3, room.Code)

	types, _ := GetRelationshipTypes(db)
	code := map[string]uint{}
	for _, ty := range types {
		code[ty.Code] = ty.ID
	}

	// Player 1 chooses TRIAD: needs two romantic partners who share a romantic
	// edge. Player 1 draws 1-2 (P) and 1-3 (D); the 2-3 edge that completes the
	// triad is drawn by player 2, so player 1 never sees it — but detection
	// still resolves over the full graph.
	var triad GoalCard
	db.Where("code = ?", "triad").First(&triad)
	SetPlayerGoal(db, room, 1, triad.ID)
	SetRelationship(db, room, 1, 2, code["P"])
	SetRelationship(db, room, 1, 3, code["D"])

	if EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("TRIAD not yet achieved before the 2-3 edge exists")
	}
	SetRelationship(db, room, 2, 3, code["P"]) // drawn by player 2
	if !EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("TRIAD should be achieved once the 2-3 romantic edge exists, even though player 1 cannot see it")
	}

	// And it surfaces in player 1's own state snapshot.
	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}
	if st.Goal == nil || !st.Goal.Achieved {
		t.Fatalf("goal.achieved should be true in player 1's state: %+v", st.Goal)
	}
	// Player 2 selected nothing → no goal in their state.
	st2, _ := BuildRoomState(db, room, 2)
	if st2.Goal != nil {
		t.Fatalf("player 2 has no goal; their state must carry none: %+v", st2.Goal)
	}
}

// No selected goal → not achieved.
func TestEvaluateGoalAchieved_NoGoal(t *testing.T) {
	db, _ := gorm.Open("sqlite3", ":memory:")
	defer db.Close()
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &GoalCard{}, &PlayerGoal{})
	SeedRelationshipTypes(db)
	SeedGoalCards(db)
	room, _, _ := CreateRoom(db, 1, 4)
	if EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("a player with no selected goal is never achieved")
	}
}
