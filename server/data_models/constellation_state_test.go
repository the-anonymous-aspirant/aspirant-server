package data_models

import (
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newStateDB(t *testing.T) (*gorm.DB, Room) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &RelationshipAction{}, &RelationshipEvent{}, &DiceRoll{}, &ConstellationProfile{})
	SeedRelationshipTypes(db)
	room, _, _ := CreateRoom(db, 1, 4)
	JoinRoom(db, 2, room.Code)
	return db, room
}

func TestBuildRoomState_ComposesEverything(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	// Member 1 has a game identity + an avatar; member 2 has neither.
	db.Create(&User{Username: "u1", Email: "u1@x", Password: "p", AvatarETag: "etag1"})
	SetConstellationUsername(db, 1, "Nova")

	types, _ := GetRelationshipTypes(db)
	partner := types[0]
	// A relationship, a dice roll, and thus some history.
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, partner.ID); err != nil {
		t.Fatalf("set: %v", err)
	}
	RollDice(db, room)

	// Viewer 1 is an endpoint of the edge set below, so it is in their view
	// (#4806 ask 2 narrows Relationships per viewer; everything else is
	// room-wide and unaffected).
	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}

	if st.Code != room.Code || st.PlayerCount != 4 || st.Occupancy != 2 {
		t.Fatalf("room fields wrong: %+v", st)
	}
	if len(st.Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(st.Members))
	}
	// Member 1 enriched with identity + avatar; member 2 empty-but-present.
	var m1, m2 RoomStateMember
	for _, m := range st.Members {
		if m.UserID == 1 {
			m1 = m
		} else if m.UserID == 2 {
			m2 = m
		}
	}
	if m1.GameUsername != "Nova" || m1.AvatarURL == "" {
		t.Fatalf("member 1 identity/avatar not composed: %+v", m1)
	}
	if m2.GameUsername != "" || m2.AvatarURL != "" {
		t.Fatalf("member 2 should have no identity/avatar: %+v", m2)
	}

	if len(st.Relationships) != 1 {
		t.Fatalf("want 1 relationship, got %d", len(st.Relationships))
	}
	r := st.Relationships[0]
	if r.TypeID != partner.ID || r.Colour != partner.Colour || r.TypeCode != partner.Code {
		t.Fatalf("edge not enriched with type colour: %+v (want colour %s)", r, partner.Colour)
	}

	if st.Dice == nil || st.Dice.Nonce == 0 || len(st.Dice.Faces) == 0 {
		t.Fatalf("dice not composed: %+v", st.Dice)
	}
	if st.HistoryCursor == 0 {
		t.Fatalf("history cursor should be non-zero after an edit")
	}
}

func TestBuildRoomState_EmptyRoom(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}
	if st.Dice != nil {
		t.Fatalf("dice should be nil before any roll, got %+v", st.Dice)
	}
	if len(st.Relationships) != 0 {
		t.Fatalf("no relationships expected, got %d", len(st.Relationships))
	}
	if st.HistoryCursor != 0 {
		t.Fatalf("history cursor should be 0 on an untouched room, got %d", st.HistoryCursor)
	}
}

func TestRoomHistoryCursor_AdvancesOnEdit(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()

	if c := RoomHistoryCursor(db, room.ID); c != 0 {
		t.Fatalf("empty room cursor want 0, got %d", c)
	}
	types, _ := GetRelationshipTypes(db)
	SetRelationshipWithHistory(db, room, 1, 1, 2, types[0].ID)
	c1 := RoomHistoryCursor(db, room.ID)
	SetRelationshipWithHistory(db, room, 1, 1, 2, types[1].ID)
	c2 := RoomHistoryCursor(db, room.ID)
	if !(c1 > 0 && c2 > c1) {
		t.Fatalf("cursor should advance on each edit: c1=%d c2=%d", c1, c2)
	}
}

// ---- #4806 ask 2: a player sees only the connections they are part of ------

// relIDs is the set of endpoint pairs in a viewer's state, for comparing views.
func relIDs(st RoomState) map[[2]uint]bool {
	out := make(map[[2]uint]bool, len(st.Relationships))
	for _, r := range st.Relationships {
		lo, hi := r.FromUserID, r.ToUserID
		if lo > hi {
			lo, hi = hi, lo
		}
		out[[2]uint{lo, hi}] = true
	}
	return out
}

// Three members, two edges: 1–2 and 2–3. Each viewer must see their own edges
// and NOT the others'. Both halves are asserted — a filter that returned
// nothing at all would satisfy a presence-only test.
func TestBuildRoomStateScopesRelationshipsToViewer(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()
	if _, _, err := JoinRoom(db, 3, room.Code); err != nil {
		t.Fatalf("join 3: %v", err)
	}
	types, _ := GetRelationshipTypes(db)
	if _, err := SetRelationshipWithHistory(db, room, 1, 1, 2, types[0].ID); err != nil {
		t.Fatalf("set 1-2: %v", err)
	}
	if _, err := SetRelationshipWithHistory(db, room, 2, 2, 3, types[1].ID); err != nil {
		t.Fatalf("set 2-3: %v", err)
	}

	pair12 := [2]uint{1, 2}
	pair23 := [2]uint{2, 3}

	for _, tc := range []struct {
		viewer  uint
		sees    [][2]uint
		notSees [][2]uint
	}{
		{viewer: 1, sees: [][2]uint{pair12}, notSees: [][2]uint{pair23}},
		{viewer: 3, sees: [][2]uint{pair23}, notSees: [][2]uint{pair12}},
		{viewer: 2, sees: [][2]uint{pair12, pair23}}, // an endpoint of both
	} {
		st, err := BuildRoomState(db, room, tc.viewer)
		if err != nil {
			t.Fatalf("BuildRoomState(viewer %d): %v", tc.viewer, err)
		}
		got := relIDs(st)
		if len(got) != len(tc.sees) {
			t.Errorf("viewer %d sees %d edges, want %d: %+v", tc.viewer, len(got), len(tc.sees), st.Relationships)
		}
		for _, p := range tc.sees {
			if !got[p] {
				t.Errorf("viewer %d cannot see their own edge %v", tc.viewer, p)
			}
		}
		for _, p := range tc.notSees {
			if got[p] {
				t.Errorf("viewer %d can see someone else's edge %v", tc.viewer, p)
			}
		}
		// The members list is NOT narrowed: an edge can never reference a
		// player the viewer does not have, and the board still shows everyone.
		if len(st.Members) != 3 {
			t.Errorf("viewer %d sees %d members, want 3 — only edges are scoped", tc.viewer, len(st.Members))
		}
	}
}

// The scoping is a serializer property, not a narrowing of the data. The whole
// graph must stay readable server-side, because goal-achievement detection
// (#4807) evaluates victory conditions over edges between OTHER players while
// each client sees less than all of them.
func TestBuildRoomStateKeepsFullGraphAvailable(t *testing.T) {
	db, room := newStateDB(t)
	defer db.Close()
	if _, _, err := JoinRoom(db, 3, room.Code); err != nil {
		t.Fatalf("join 3: %v", err)
	}
	types, _ := GetRelationshipTypes(db)
	SetRelationshipWithHistory(db, room, 1, 1, 2, types[0].ID)
	SetRelationshipWithHistory(db, room, 2, 2, 3, types[1].ID)

	// A viewer sees one edge...
	st, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("BuildRoomState: %v", err)
	}
	if len(st.Relationships) != 1 {
		t.Fatalf("viewer 1 should see 1 edge, got %d", len(st.Relationships))
	}
	// ...while the graph itself still holds both.
	all, err := RoomRelationships(db, room.ID)
	if err != nil {
		t.Fatalf("RoomRelationships: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("the underlying graph should still hold both edges, got %d", len(all))
	}
}

func TestViewerSeesRelationship(t *testing.T) {
	edge := Relationship{FromUserID: 4, ToUserID: 7}
	for _, tc := range []struct {
		viewer uint
		want   bool
	}{{4, true}, {7, true}, {5, false}, {0, false}} {
		if got := ViewerSeesRelationship(edge, tc.viewer); got != tc.want {
			t.Errorf("ViewerSeesRelationship(4-7, viewer %d) = %v, want %v", tc.viewer, got, tc.want)
		}
	}
}

// A player's selected goal is PRIVATE: it appears in that player's own /state
// snapshot and never in another player's (#4807-A1). The scoping is server-side
// so a client cannot leak it via devtools — same discipline as the edge filter.
func TestBuildRoomState_GoalIsPrivate(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.AutoMigrate(&User{}, &Room{}, &RoomMember{}, &RelationshipType{},
		&Relationship{}, &RelationshipAction{}, &RelationshipEvent{}, &DiceRoll{}, &ConstellationProfile{},
		&GoalCard{}, &PlayerGoal{})
	SeedRelationshipTypes(db)
	SeedGoalCards(db)
	room, _, _ := CreateRoom(db, 1, 4)
	JoinRoom(db, 2, room.Code)

	cards, _ := GetGoalCards(db)
	chosen := cards[0]
	if _, err := SetPlayerGoal(db, room, 1, chosen.ID); err != nil {
		t.Fatalf("SetPlayerGoal: %v", err)
	}

	// Player 1 sees their own goal.
	own, err := BuildRoomState(db, room, 1)
	if err != nil {
		t.Fatalf("BuildRoomState(1): %v", err)
	}
	if own.Goal == nil || own.Goal.CardID != chosen.ID {
		t.Fatalf("player 1's own goal missing from their state: %+v", own.Goal)
	}
	if own.Goal.VictoryCondition == "" || own.Goal.Achieved {
		t.Errorf("goal payload wrong: condition=%q achieved=%v (achieved is A2's job)", own.Goal.VictoryCondition, own.Goal.Achieved)
	}

	// Player 2 must NOT see player 1's goal.
	other, err := BuildRoomState(db, room, 2)
	if err != nil {
		t.Fatalf("BuildRoomState(2): %v", err)
	}
	if other.Goal != nil {
		t.Fatalf("player 1's goal leaked into player 2's state: %+v", other.Goal)
	}
}
