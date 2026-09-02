package data_models

import "github.com/jinzhu/gorm"

// Constellations append-only relationship-event log (epic #4807, subtask
// #4829-A3).
//
// This is a GENERAL, UNCAPPED record of every edge state change in a room, with
// a monotonic ordering — distinct from the #4599 undo stack
// (constellation_relationship_actions), which is per-player, capped at
// HistoryCapPerPlayer=20, and stores prev-state for undo. Per the operator's Q4
// ruling (c27322: "trace ALL history") the log must survive an arbitrarily long
// game, so it has no cap and is never truncated; the two history-dependent goal
// predicates (unicorn hunter, THE ESCALATOR) read it in
// constellation_goal_detection.go.
//
// Ordering is the row's own gorm.Model.ID (a monotonic autoincrement) — reads
// use `ORDER BY id ASC` for a stable oldest-first timeline. The log is append
// only: undo/redo do NOT delete rows, they append a new event for the state
// they restore, so "escalated at some point" stays true even after an undo.
//
// The pair is stored normalized (PairLow <= PairHigh) so an edit and its
// reverse address the same unordered pair regardless of click order, matching
// the one-active-edge-per-pair invariant of the base relationship layer.
type RelationshipEvent struct {
	gorm.Model
	RoomID      uint                   `json:"room_id" gorm:"not null;index"`
	ActorUserID uint                   `json:"actor_user_id" gorm:"not null;index"`
	PairLow     uint                   `json:"pair_low" gorm:"not null;index"`
	PairHigh    uint                   `json:"pair_high" gorm:"not null;index"`
	Kind        RelationshipActionKind `json:"kind" gorm:"not null"` // "set" | "clear"
	TypeID      uint                   `json:"type_id"`              // the type set (0 for a clear)
	FromUserID  uint                   `json:"from_user_id"`         // direction the edge was set in (0 for a clear)
	ToUserID    uint                   `json:"to_user_id"`
}

// TableName pins the physical table name.
func (RelationshipEvent) TableName() string { return "constellation_relationship_events" }

// recordEvent appends one edge-change event to the log. Unlike recordAction it
// enforces no cap and never deletes prior rows — the log is append-only. Called
// after a successful edit at every state-change choke point (set / clear /
// undo / redo) in constellation_relationship_action.go.
func recordEvent(db *gorm.DB, roomID, actor uint, kind RelationshipActionKind, typeID, from, to uint) error {
	low, high := normPair(from, to)
	e := RelationshipEvent{
		RoomID: roomID, ActorUserID: actor, PairLow: low, PairHigh: high,
		Kind: kind, TypeID: typeID, FromUserID: from, ToUserID: to,
	}
	return db.Create(&e).Error
}

// RoomRelationshipEvents returns the room's full event log oldest-first — the
// ordered timeline the history-dependent goal predicates evaluate over.
func RoomRelationshipEvents(db *gorm.DB, roomID uint) ([]RelationshipEvent, error) {
	var events []RelationshipEvent
	err := db.Where("room_id = ?", roomID).Order("id ASC").Find(&events).Error
	return events, err
}

// RoomRelationshipEventsPage returns one oldest-first page of the room's event
// log: up to limit rows with ID > afterID (afterID 0 starts at the beginning).
// The log is append-only, so an id cursor is stable — rows are only ever added
// after the last one a prior page saw. Deliberately UNSCOPED: the #4847 room
// history endpoint filters for its viewer on the way out (in the handler), so
// this read stays usable by features that need the whole timeline, exactly as
// RoomRelationshipEvents is for the goal predicates.
func RoomRelationshipEventsPage(db *gorm.DB, roomID, afterID uint, limit int) ([]RelationshipEvent, error) {
	var events []RelationshipEvent
	err := db.Where("room_id = ? AND id > ?", roomID, afterID).
		Order("id ASC").Limit(limit).Find(&events).Error
	return events, err
}
