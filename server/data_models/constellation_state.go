package data_models

import (
	"github.com/jinzhu/gorm"
)

// Constellations room live-state snapshot (epic #4587, subtask #4600-D1).
//
// One aggregate read composing everything a short-poll board client needs each
// ~1-2s tick: A1 room + seats, A3 game identity + the existing avatar, B1
// relationship edges enriched with A2 type colour, B2 latest dice roll, and the
// C1 history cursor. Read-only; no new schema. The sync mechanism is short-poll
// (engineer decision — no push infra exists), so the shape is deliberately
// self-contained: the client re-renders from one response without a second call.

// RoomStateMember is a seated player enriched with game identity.
type RoomStateMember struct {
	UserID       uint   `json:"user_id"`
	Slot         int    `json:"slot"`
	GameUsername string `json:"game_username"`
	AvatarURL    string `json:"avatar_url"`
}

// RoomStateRelationship is a shared-graph edge carrying its type colour, so the
// client renders the coloured line without joining the vocabulary itself.
type RoomStateRelationship struct {
	FromUserID uint   `json:"from_user_id"`
	ToUserID   uint   `json:"to_user_id"`
	TypeID     uint   `json:"type_id"`
	TypeCode   string `json:"type_code"`
	TypeLabel  string `json:"type_label"`
	Colour     string `json:"colour"`
}

// RoomStateDice is the latest resolved roll everyone converges on (B2).
type RoomStateDice struct {
	Faces    []int  `json:"faces"`
	Nonce    uint   `json:"nonce"`
	RolledAt string `json:"rolled_at,omitempty"`
}

// RoomStateGoal is the viewer's OWN selected goal card (#4807-A1). It is private
// to the viewer: BuildRoomState populates it only from the viewer's own
// PlayerGoal and never from another player's, so a goal never appears in anyone
// else's snapshot. Achieved is filled by the detection engine (#4807-A2); until
// that lands it stays false.
type RoomStateGoal struct {
	CardID           uint   `json:"card_id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	VictoryCondition string `json:"victory_condition"`
	Achieved         bool   `json:"achieved"`
}

// RoomState is the board snapshot returned by GET .../rooms/:code/state, as
// seen BY ONE VIEWER. Everything in it is room-wide except Relationships, which
// is scoped to the viewer (see BuildRoomState).
type RoomState struct {
	Code          string                  `json:"code"`
	PlayerCount   int                     `json:"player_count"`
	Status        string                  `json:"status"`
	Occupancy     int                     `json:"occupancy"`
	Members       []RoomStateMember       `json:"members"`
	Relationships []RoomStateRelationship `json:"relationships"`
	Dice          *RoomStateDice          `json:"dice"`
	Goal          *RoomStateGoal          `json:"goal"`
	HistoryCursor uint                    `json:"history_cursor"`
}

// avatarETags batch-reads the avatar content ETags for a set of user ids, so
// BuildRoomState resolves every member's avatar URL in one query.
func avatarETags(db *gorm.DB, ids []uint) map[uint]string {
	out := make(map[uint]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	var rows []User
	// Read via the struct field so gorm resolves the physical column name
	// (AvatarETag does not map to a literal "avatar_etag").
	db.Where("id IN (?)", ids).Find(&rows)
	for _, u := range rows {
		out[u.ID] = u.AvatarETag
	}
	return out
}

// ViewerSeesRelationship reports whether viewerUserID may see edge r — true
// exactly when they are one of its two endpoints (#4806 ask 2: "I'd prefer it
// if the lines that appear between people is not visible to other players").
//
// Endpoint scoping, not author scoping. The connection graph stays SHARED and
// single — one active edge per unordered pair — because the goal cards the
// operator supplied (#4807) are victory conditions evaluated over that one
// graph ("to obtain two partners; these partners must not have date/partner/F+
// relationships with anyone but you"). Per-author edges would let two players
// hold contradictory connections for the same pair and leave no ground truth
// for a condition to be true of. What ask 2 hides is who ELSE is connected to
// whom — the hidden information that makes those goals a game.
//
// This is a SERIALIZER property, deliberately. RoomRelationships keeps
// returning the whole graph, so server-side goal detection reads ground truth
// while each client sees less than all of it (#4807 c27156 §3), and the
// operator's "for now" on privacy stays a one-line change to reverse.
func ViewerSeesRelationship(r Relationship, viewerUserID uint) bool {
	return r.FromUserID == viewerUserID || r.ToUserID == viewerUserID
}

// BuildRoomState composes the board snapshot for an active room as seen by
// viewerUserID. Every field is room-wide except Relationships, which carries
// only the edges that viewer is an endpoint of.
func BuildRoomState(db *gorm.DB, room Room, viewerUserID uint) (RoomState, error) {
	members, err := RoomMembers(db, room.ID)
	if err != nil {
		return RoomState{}, err
	}
	ids := make([]uint, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	etags := avatarETags(db, ids)

	stateMembers := make([]RoomStateMember, 0, len(members))
	for _, m := range members {
		username := ""
		if p, ok := GetConstellationProfile(db, m.UserID); ok {
			username = p.GameUsername
		}
		stateMembers = append(stateMembers, RoomStateMember{
			UserID:       m.UserID,
			Slot:         m.Slot,
			GameUsername: username,
			AvatarURL:    AvatarURLFor(m.UserID, etags[m.UserID]),
		})
	}

	// Type colour map for enriching each edge (A2 vocabulary).
	types, err := GetRelationshipTypes(db)
	if err != nil {
		return RoomState{}, err
	}
	typeByID := make(map[uint]RelationshipType, len(types))
	for _, t := range types {
		typeByID[t.ID] = t
	}

	rels, err := RoomRelationships(db, room.ID)
	if err != nil {
		return RoomState{}, err
	}
	stateRels := make([]RoomStateRelationship, 0, len(rels))
	for _, r := range rels {
		// The filter is here, on the way out — not in the query above, which
		// stays the whole-graph read (see ViewerSeesRelationship).
		if !ViewerSeesRelationship(r, viewerUserID) {
			continue
		}
		t := typeByID[r.TypeID]
		stateRels = append(stateRels, RoomStateRelationship{
			FromUserID: r.FromUserID,
			ToUserID:   r.ToUserID,
			TypeID:     r.TypeID,
			TypeCode:   t.Code,
			TypeLabel:  t.Label,
			Colour:     t.Colour,
		})
	}

	var dice *RoomStateDice
	if roll, ok := CurrentRoll(db, room.ID); ok {
		dice = &RoomStateDice{
			Faces:    roll.FaceValues(),
			Nonce:    roll.ID,
			RolledAt: roll.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}

	// The viewer's OWN goal only — private, like the edge filter above. Another
	// player's goal is never read here, so it cannot leak into this snapshot
	// (#4807-A1; privacy is a serializer property so "for now" is reversible).
	var goal *RoomStateGoal
	if card, ok := GetPlayerGoal(db, room.ID, viewerUserID); ok {
		goal = &RoomStateGoal{
			CardID:           card.ID,
			Code:             card.Code,
			Name:             card.Name,
			VictoryCondition: card.VictoryCondition,
			// Detection reads the FULL room graph (#4826-A2); the player sees
			// only their own edges above, but the win is judged over everyone's.
			Achieved: EvaluateGoalAchieved(db, room, viewerUserID),
		}
	}

	return RoomState{
		Code:          room.Code,
		PlayerCount:   room.PlayerCount,
		Status:        room.Status,
		Occupancy:     RoomOccupancy(db, room.ID),
		Members:       stateMembers,
		Relationships: stateRels,
		Dice:          dice,
		Goal:          goal,
		HistoryCursor: RoomHistoryCursor(db, room.ID),
	}, nil
}
