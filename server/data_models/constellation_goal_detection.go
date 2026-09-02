package data_models

import "github.com/jinzhu/gorm"

// Constellations goal-achievement detection (epic #4807, subtask #4826-A2).
//
// The 16 goal cards (A1) are victory conditions over the room's connection
// graph. Detection is SERVER-SIDE and reads the FULL graph (RoomRelationships,
// not the per-viewer serializer scope from #4809/#4807) — the server sees
// everything, each client sees only its own edges, so a goal can be achieved off
// edges the player cannot see. Two of the 16 cards (unicorn hunter, THE
// ESCALATOR) need relationship history and are evaluated in #4829-A3; their
// predicate keys return false here.
//
// Connection type codes (A2 vocabulary): P Partner, D Date, F+ Friends with
// benefits, F Friend, A Affair, R Rejection.

// roomGraph is a snapshot of a room's active connection graph, keyed for the
// queries the predicates need: the type of the edge between any two players, and
// the set of a player's neighbours by type. The graph is undirected — the edge
// invariant is one active edge per unordered pair — so edges are stored under a
// canonical (low,high) key and read from either direction.
type roomGraph struct {
	edges       map[[2]uint]string // canonical unordered pair -> type code
	players     []uint
	playerCount int // configured game size (room.PlayerCount)

	// events is the room's full append-only edge-change timeline (A3), oldest
	// first, each resolved to its type code. Snapshot predicates ignore it; the
	// two history-dependent predicates (unicorn hunter, THE ESCALATOR) read it.
	// A `set` event carries the code it set; a `clear` event carries "".
	events []edgeEvent
}

// edgeEvent is one entry of the append-only relationship-event log resolved for
// predicate evaluation: which unordered pair changed and to what type code.
type edgeEvent struct {
	pair [2]uint // canonical (low, high)
	code string  // type code set; "" for a clear
}

func pairKey(a, b uint) [2]uint {
	if a <= b {
		return [2]uint{a, b}
	}
	return [2]uint{b, a}
}

// edgeType returns the connection type code between a and b, or "" if there is
// no active edge.
func (g *roomGraph) edgeType(a, b uint) string { return g.edges[pairKey(a, b)] }

// inSet reports membership in a small type-code set.
func inSet(code string, set ...string) bool {
	for _, s := range set {
		if code == s {
			return true
		}
	}
	return false
}

// neighbours returns the players connected to me by an edge whose type is in the
// given set.
func (g *roomGraph) neighbours(me uint, types ...string) []uint {
	out := []uint{}
	for _, p := range g.players {
		if p == me {
			continue
		}
		if inSet(g.edgeType(me, p), types...) {
			out = append(out, p)
		}
	}
	return out
}

// count is the number of distinct players connected to me by an edge in the set.
func (g *roomGraph) count(me uint, types ...string) int { return len(g.neighbours(me, types...)) }

// buildRoomGraph loads the active edges and resolves each to its type code.
func buildRoomGraph(db *gorm.DB, room Room) (*roomGraph, error) {
	types, err := GetRelationshipTypes(db)
	if err != nil {
		return nil, err
	}
	codeByID := make(map[uint]string, len(types))
	for _, t := range types {
		codeByID[t.ID] = t.Code
	}

	rels, err := RoomRelationships(db, room.ID)
	if err != nil {
		return nil, err
	}
	members, err := RoomMembers(db, room.ID)
	if err != nil {
		return nil, err
	}

	events, err := RoomRelationshipEvents(db, room.ID)
	if err != nil {
		return nil, err
	}

	g := &roomGraph{edges: make(map[[2]uint]string), playerCount: room.PlayerCount}
	for _, m := range members {
		g.players = append(g.players, m.UserID)
	}
	for _, r := range rels {
		if code, ok := codeByID[r.TypeID]; ok {
			g.edges[pairKey(r.FromUserID, r.ToUserID)] = code
		}
	}
	for _, e := range events {
		code := ""
		if e.Kind == ActionSet {
			code = codeByID[e.TypeID] // "" if the type is unknown; a clear also yields ""
		}
		g.events = append(g.events, edgeEvent{pair: pairKey(e.PairLow, e.PairHigh), code: code})
	}
	return g, nil
}

// EvaluateGoalAchieved reports whether the player's currently selected goal is
// satisfied by the room's connection graph right now. A player with no selected
// goal, or a goal whose predicate is history-dependent (evaluated in A3), yields
// false.
func EvaluateGoalAchieved(db *gorm.DB, room Room, userID uint) bool {
	card, ok := GetPlayerGoal(db, room.ID, userID)
	if !ok {
		return false
	}
	g, err := buildRoomGraph(db, room)
	if err != nil {
		return false
	}
	fn, ok := goalPredicates[card.PredicateKey]
	if !ok {
		return false
	}
	return fn(g, userID)
}

// goalPredicates maps each snapshot-evaluable card to its predicate. The two
// history-dependent cards are intentionally absent (A3 owns them), so they
// resolve to false here rather than a wrong snapshot answer.
var goalPredicates = map[string]func(g *roomGraph, me uint) bool{
	"v_two_unshared":                          predV,
	"triad_two_shared":                        predTriad,
	"quad_three_no_rejection":                 predQuad,
	"monogamy_exclusive":                      predMonogamy,
	"single_friends_rejections":               predSingle,
	"kitchen_table_connected":                 predKitchenTable,
	"unethical_non_monogamy_one_affair":       predUnethicalNonMonogamy,
	"hierarchical_one_each":                   predHierarchical,
	"polygamy_two_exclusive_partners":         predPolygamy,
	"relationship_anarchy_three_no_rejection": predRelationshipAnarchy,
	"the_cheater_one_two_affairs":             predCheater,
	"open_relationship_three_no_rejection":    predOpenRelationship,
	"unicorn_two_shared_dates":                predUnicorn,
	"unethical_polycurious_two_one_affair":    predPolycurious,

	// History-dependent (A3): read g.events rather than only the snapshot.
	"unicorn_hunter_partner_then_date": predUnicornHunter,
	"escalator_two_with_escalation":    predEscalator,
}

// romantic is the set of "dating" edge types the cards repeatedly refer to as
// "date/partner/F+".
var romantic = []string{"P", "D", "F+"}

// twoWith returns two distinct neighbours of me (by the given types) whose
// connecting edge satisfies pred(edgeCode). It is the shared shape of V, TRIAD
// and UNICORN: pick two of my partners and test the edge between them.
func twoWith(g *roomGraph, me uint, neighbourTypes []string, pred func(edge string) bool) bool {
	ns := g.neighbours(me, neighbourTypes...)
	for i := 0; i < len(ns); i++ {
		for j := i + 1; j < len(ns); j++ {
			if pred(g.edgeType(ns[i], ns[j])) {
				return true
			}
		}
	}
	return false
}

// V: two F+/D/P who do NOT share a D/P/F+ edge with each other.
func predV(g *roomGraph, me uint) bool {
	return twoWith(g, me, romantic, func(e string) bool { return !inSet(e, romantic...) })
}

// TRIAD: two F+/D/P who DO share a D/P/F+ edge with each other.
func predTriad(g *roomGraph, me uint) bool {
	return twoWith(g, me, romantic, func(e string) bool { return inSet(e, romantic...) })
}

// UNICORN: two dates who share a F+/D/P relationship.
func predUnicorn(g *roomGraph, me uint) bool {
	return twoWith(g, me, []string{"D"}, func(e string) bool { return inSet(e, romantic...) })
}

// quad: three dates/partners with no rejection between any pair of them.
// Playable only at 6+ players.
func predQuad(g *roomGraph, me uint) bool {
	if g.playerCount < 6 {
		return false
	}
	ns := g.neighbours(me, "D", "P")
	return threeNoRejection(g, ns)
}

// threeNoRejection reports whether some three of the given players are pairwise
// free of a rejection edge.
func threeNoRejection(g *roomGraph, ns []uint) bool {
	for i := 0; i < len(ns); i++ {
		for j := i + 1; j < len(ns); j++ {
			for k := j + 1; k < len(ns); k++ {
				if g.edgeType(ns[i], ns[j]) != "R" &&
					g.edgeType(ns[i], ns[k]) != "R" &&
					g.edgeType(ns[j], ns[k]) != "R" {
					return true
				}
			}
		}
	}
	return false
}

// friendTarget is the friend/rejection count a game of this size asks for on the
// monogamy / SINGLE cards: one at exactly four players, two at five or more.
func sizeTarget(playerCount int) int {
	if playerCount <= 4 {
		return 1
	}
	return 2
}

// monogamy (exact): one partner and one/two friends, and no other dates/F+/
// partners; and that partner has no date/partner/F+ with anyone but me.
func predMonogamy(g *roomGraph, me uint) bool {
	partners := g.neighbours(me, "P")
	if len(partners) != 1 {
		return false
	}
	if g.count(me, "F") != sizeTarget(g.playerCount) {
		return false
	}
	// "no other dates/F+/partners": exactly one partner (checked) and zero D/F+.
	if g.count(me, "D") != 0 || g.count(me, "F+") != 0 {
		return false
	}
	// The partner is exclusive: no date/partner/F+ with anyone but me.
	p := partners[0]
	for _, other := range g.players {
		if other == p || other == me {
			continue
		}
		if inSet(g.edgeType(p, other), romantic...) {
			return false
		}
	}
	return true
}

// SINGLE (exact): two friends and one/two rejections, and no F+/dates/partners/
// affairs.
func predSingle(g *roomGraph, me uint) bool {
	if g.count(me, "F") != 2 {
		return false
	}
	if g.count(me, "R") != sizeTarget(g.playerCount) {
		return false
	}
	return g.count(me, "F+", "D", "P", "A") == 0
}

// KITCHEN TABLE POLYAMORY: at least two dates/partners/F+, all pairwise
// reachable through D/P/F+ edges (one connected component) and with no rejection
// among them.
func predKitchenTable(g *roomGraph, me uint) bool {
	ns := g.neighbours(me, romantic...)
	if len(ns) < 2 {
		return false
	}
	// No rejection among the partners.
	for i := 0; i < len(ns); i++ {
		for j := i + 1; j < len(ns); j++ {
			if g.edgeType(ns[i], ns[j]) == "R" {
				return false
			}
		}
	}
	// Connected through D/P/F+ edges (BFS over the partner set).
	inGroup := make(map[uint]bool, len(ns))
	for _, p := range ns {
		inGroup[p] = true
	}
	seen := map[uint]bool{ns[0]: true}
	queue := []uint{ns[0]}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, p := range ns {
			if !seen[p] && inSet(g.edgeType(cur, p), romantic...) {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return len(seen) == len(ns)
}

// UNETHICAL NON-MONOGAMY: one F+/date/partner and one affair.
func predUnethicalNonMonogamy(g *roomGraph, me uint) bool {
	return g.count(me, romantic...) >= 1 && g.count(me, "A") >= 1
}

// HIERARCHICAL POLYAMORY: one F+, one date, one partner, no duplicate of a type
// aside from friends. Playable only at 6+ players.
func predHierarchical(g *roomGraph, me uint) bool {
	if g.playerCount < 6 {
		return false
	}
	return g.count(me, "P") == 1 &&
		g.count(me, "D") == 1 &&
		g.count(me, "F+") == 1 &&
		g.count(me, "A") <= 1
}

// polygamy: at least two partners, none of whom has a date/partner/F+ with
// anyone but me.
func predPolygamy(g *roomGraph, me uint) bool {
	partners := g.neighbours(me, "P")
	if len(partners) < 2 {
		return false
	}
	for _, p := range partners {
		for _, other := range g.players {
			if other == p || other == me {
				continue
			}
			if inSet(g.edgeType(p, other), romantic...) {
				return false
			}
		}
	}
	return true
}

// RELATIONSHIP ANARCHY: a relationship with at least three other players (any of
// P/D/F+/F), and no rejection on me.
func predRelationshipAnarchy(g *roomGraph, me uint) bool {
	return g.count(me, "P", "D", "F+", "F") >= 3 && g.count(me, "R") == 0
}

// THE CHEATER: one F+/date/partner and two affairs.
func predCheater(g *roomGraph, me uint) bool {
	return g.count(me, romantic...) >= 1 && g.count(me, "A") >= 2
}

// open relationship: three F+/dates/partners and no rejection on me.
func predOpenRelationship(g *roomGraph, me uint) bool {
	return g.count(me, romantic...) >= 3 && g.count(me, "R") == 0
}

// UNETHICAL POLYCURIOUS: two F+/dates/partners and one affair (Q5 — partners
// count, per operator override of the literal card text).
func predPolycurious(g *roomGraph, me uint) bool {
	return g.count(me, romantic...) >= 2 && g.count(me, "A") >= 1
}

// --- History-dependent predicates (#4829-A3), evaluated over g.events ---

// allEdgeTypes is every connection type an active edge can carry — used by THE
// ESCALATOR's "two relationships of any kind" count.
var allEdgeTypes = []string{"P", "D", "F+", "F", "A", "R"}

// firstSetIndex is the index in g.events of the first event that set the pair
// {me, other} to code, or -1 if it never did. Because the log is ordered
// oldest-first, a smaller index means "obtained earlier".
func (g *roomGraph) firstSetIndex(me, other uint, code string) int {
	want := pairKey(me, other)
	for i, e := range g.events {
		if e.pair == want && e.code == code {
			return i
		}
	}
	return -1
}

// ladderRank orders the escalation chain F < F+ < D < P; non-ladder codes
// (Affair, Rejection, a clear) return 0 so they neither start nor advance an
// escalation.
func ladderRank(code string) int {
	switch code {
	case "F":
		return 1
	case "F+":
		return 2
	case "D":
		return 3
	case "P":
		return 4
	default:
		return 0
	}
}

// pairEscalated reports whether the pair {me, other}'s recorded history contains
// a strict up-step along the ladder — some later `set` event with a higher
// ladder rank than an earlier one on the same pair. Clears and non-ladder
// events are skipped, so an intervening clear does not reset the "escalated at
// some point" reading; the log is append-only, so an undo of an escalation
// leaves the earlier up-step in place.
func (g *roomGraph) pairEscalated(me, other uint) bool {
	want := pairKey(me, other)
	minPrior := 1 << 30
	for _, e := range g.events {
		if e.pair != want {
			continue
		}
		r := ladderRank(e.code)
		if r == 0 {
			continue
		}
		if r > minPrior {
			return true
		}
		if r < minPrior {
			minPrior = r
		}
	}
	return false
}

// UNICORN HUNTER: obtain a partner FIRST, then a date, where the partner and the
// date are themselves dating (their edge is in {P,D,F+}). Snapshot-current
// partner X and date Y, ordered by the log so the P was set before the D.
func predUnicornHunter(g *roomGraph, me uint) bool {
	partners := g.neighbours(me, "P")
	dates := g.neighbours(me, "D")
	for _, x := range partners {
		fp := g.firstSetIndex(me, x, "P")
		if fp < 0 {
			continue
		}
		for _, y := range dates {
			if y == x {
				continue
			}
			fd := g.firstSetIndex(me, y, "D")
			if fd < 0 || fp >= fd {
				continue
			}
			if inSet(g.edgeType(x, y), romantic...) {
				return true
			}
		}
	}
	return false
}

// THE ESCALATOR: two relationships of any kind, one of which climbed the ladder
// F -> F+ -> D -> P at some point. Requires at least two active edges now, and
// at least one currently-active pair whose history shows an up-step.
func predEscalator(g *roomGraph, me uint) bool {
	if g.count(me, allEdgeTypes...) < 2 {
		return false
	}
	for _, p := range g.players {
		if p == me {
			continue
		}
		if g.edgeType(me, p) == "" {
			continue
		}
		if g.pairEscalated(me, p) {
			return true
		}
	}
	return false
}
