package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

// evented attaches an ordered event timeline to a hand-laid graph so the two
// history-dependent predicates can be tested without a DB. `edge` (a, b, code)
// is reused as an event; a code of "" is a clear event.
func evented(g *roomGraph, evs ...edge) *roomGraph {
	for _, e := range evs {
		g.events = append(g.events, edgeEvent{pair: pairKey(e.a, e.b), code: e.code})
	}
	return g
}

func TestPredUnicornHunter(t *testing.T) {
	// Partner (1-2) obtained FIRST, date (1-3) second, and the partner and date
	// are dating (2-3 romantic) → satisfied.
	g := evented(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "P"}),
		edge{1, 2, "P"}, edge{1, 3, "D"})
	if !predUnicornHunter(g, 1) {
		t.Error("unicorn hunter: partner-then-date with a dating partner+date should satisfy")
	}

	// Date obtained first, partner second → order fails.
	g = evented(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "P"}),
		edge{1, 3, "D"}, edge{1, 2, "P"})
	if predUnicornHunter(g, 1) {
		t.Error("unicorn hunter: date obtained before partner must NOT satisfy (partner must be first)")
	}

	// Right order, but the partner and date are not dating (2-3 is a friendship).
	g = evented(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "D"}, edge{2, 3, "F"}),
		edge{1, 2, "P"}, edge{1, 3, "D"})
	if predUnicornHunter(g, 1) {
		t.Error("unicorn hunter: the partner and date must themselves be dating (P/D/F+)")
	}

	// A partner but no current date → nothing to pair.
	g = evented(graph(4, p6, edge{1, 2, "P"}), edge{1, 2, "P"})
	if predUnicornHunter(g, 1) {
		t.Error("unicorn hunter: a partner with no date must NOT satisfy")
	}
}

func TestPredEscalator(t *testing.T) {
	// Two relationships; the 1-2 edge climbed F -> P → satisfied.
	g := evented(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "F"}),
		edge{1, 2, "F"}, edge{1, 2, "P"}, edge{1, 3, "F"})
	if !predEscalator(g, 1) {
		t.Error("escalator: two edges with one climbing F->P should satisfy")
	}

	// Two edges, but neither ever climbed the ladder.
	g = evented(graph(4, p6, edge{1, 2, "F"}, edge{1, 3, "D"}),
		edge{1, 2, "F"}, edge{1, 3, "D"})
	if predEscalator(g, 1) {
		t.Error("escalator: two flat edges with no up-step must NOT satisfy")
	}

	// Only one relationship, even if it escalated → needs two.
	g = evented(graph(4, p6, edge{1, 2, "P"}), edge{1, 2, "F"}, edge{1, 2, "P"})
	if predEscalator(g, 1) {
		t.Error("escalator: a single escalated edge must NOT satisfy (needs two relationships)")
	}

	// An intervening clear does not reset the escalation ("at some point"): the
	// 1-2 pair went F, was cleared, then re-set to P — still an up-step.
	g = evented(graph(4, p6, edge{1, 2, "P"}, edge{1, 3, "F"}),
		edge{1, 2, "F"}, edge{1, 2, ""}, edge{1, 2, "P"}, edge{1, 3, "F"})
	if !predEscalator(g, 1) {
		t.Error("escalator: an intervening clear must NOT reset the escalation")
	}
}

// newEventDB migrates the relationship + event + goal tables and seeds a room
// with three members (1 host, 2 and 3 joined).
func newEventDB(t *testing.T) (*gorm.DB, Room, map[string]uint) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &RelationshipAction{}, &RelationshipEvent{},
		&GoalCard{}, &PlayerGoal{})
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
	return db, room, code
}

// The event log records every state change in order and is never capped.
func TestRelationshipEventLogRecordsAndIsUncapped(t *testing.T) {
	db, room, code := newEventDB(t)
	defer db.Close()

	// set -> change -> clear, all on the 1-2 pair by player 1.
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, code["F"]); err != nil {
		t.Fatalf("set F: %v", err)
	}
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, code["P"]); err != nil {
		t.Fatalf("set P: %v", err)
	}
	if err := ClearRelationshipWithHistory(db, room, 1, 1, 2); err != nil {
		t.Fatalf("clear: %v", err)
	}
	events, err := RoomRelationshipEvents(db, room.ID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Kind != ActionSet || events[0].TypeID != code["F"] {
		t.Errorf("event 0 should be set F, got kind=%v type=%d", events[0].Kind, events[0].TypeID)
	}
	if events[1].Kind != ActionSet || events[1].TypeID != code["P"] {
		t.Errorf("event 1 should be set P, got kind=%v type=%d", events[1].Kind, events[1].TypeID)
	}
	if events[2].Kind != ActionClear || events[2].TypeID != 0 {
		t.Errorf("event 2 should be clear, got kind=%v type=%d", events[2].Kind, events[2].TypeID)
	}

	// Uncapped: drive many edits on one pair; the per-player action stack caps at
	// HistoryCapPerPlayer, but the event log keeps every one.
	total := HistoryCapPerPlayer + 5
	for i := 0; i < total; i++ {
		c := code["F"]
		if i%2 == 1 {
			c = code["P"]
		}
		if _, err := SetRelationshipWithHistory(db, room, 1, 1, 3, c); err != nil {
			t.Fatalf("bulk set %d: %v", i, err)
		}
	}
	var pairEvents int
	all, _ := RoomRelationshipEvents(db, room.ID)
	low, high := normPair(1, 3)
	for _, e := range all {
		if e.PairLow == low && e.PairHigh == high {
			pairEvents++
		}
	}
	if pairEvents != total {
		t.Errorf("event log must be uncapped: expected %d events for the 1-3 pair, got %d", total, pairEvents)
	}
	// And the action stack for that player/pair is still capped.
	actions, _ := PlayerHistory(db, room.ID, 1)
	if len(actions) > HistoryCapPerPlayer {
		t.Errorf("action stack should stay capped at %d, got %d", HistoryCapPerPlayer, len(actions))
	}
}

// Undo/redo also append events, so history remains a faithful timeline.
func TestRelationshipEventLogCapturesUndoRedo(t *testing.T) {
	db, room, code := newEventDB(t)
	defer db.Close()

	SetRelationshipWithHistory(db, room, 1, 1, 2, code["F"]) // event 1: set F
	SetRelationshipWithHistory(db, room, 1, 1, 2, code["P"]) // event 2: set P
	if err := UndoRelationship(db, room, 1); err != nil {    // event 3: restore F
		t.Fatalf("undo: %v", err)
	}
	if err := RedoRelationship(db, room, 1); err != nil { // event 4: re-apply P
		t.Fatalf("redo: %v", err)
	}
	events, _ := RoomRelationshipEvents(db, room.ID)
	if len(events) != 4 {
		t.Fatalf("expected 4 events (set,set,undo,redo), got %d", len(events))
	}
	if events[2].TypeID != code["F"] {
		t.Errorf("undo event should restore F, got type=%d", events[2].TypeID)
	}
	if events[3].TypeID != code["P"] {
		t.Errorf("redo event should re-apply P, got type=%d", events[3].TypeID)
	}
}

// End-to-end through the real mutators + detection engine.
func TestEvaluateGoalAchieved_UnicornHunter(t *testing.T) {
	db, room, code := newEventDB(t)
	defer db.Close()

	var card GoalCard
	db.Where("code = ?", "unicorn_hunter").First(&card)
	SetPlayerGoal(db, room, 1, card.ID)

	// Partner first, then date; the partner (2) and date (3) are dating.
	SetRelationshipWithHistory(db, room, 1, 1, 2, code["P"])
	SetRelationshipWithHistory(db, room, 1, 1, 3, code["D"])
	if EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("unicorn hunter: not achieved before the partner and date are dating")
	}
	SetRelationshipWithHistory(db, room, 2, 2, 3, code["P"])
	if !EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("unicorn hunter: should be achieved once partner-then-date and 2-3 are dating")
	}
	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}
	if st.Goal == nil || !st.Goal.Achieved {
		t.Fatalf("goal.achieved should be true in player 1's state: %+v", st.Goal)
	}
}

func TestEvaluateGoalAchieved_Escalator(t *testing.T) {
	db, room, code := newEventDB(t)
	defer db.Close()

	var card GoalCard
	db.Where("code = ?", "the_escalator").First(&card)
	SetPlayerGoal(db, room, 1, card.ID)

	// One relationship that climbs F -> P, and a second relationship.
	SetRelationshipWithHistory(db, room, 1, 1, 2, code["F"])
	if EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("escalator: not achieved with a single un-escalated edge")
	}
	SetRelationshipWithHistory(db, room, 1, 1, 2, code["P"]) // escalate 1-2
	SetRelationshipWithHistory(db, room, 1, 1, 3, code["F"]) // second relationship
	if !EvaluateGoalAchieved(db, room, 1) {
		t.Fatal("escalator: should be achieved with two edges, one having climbed F->P")
	}
}
