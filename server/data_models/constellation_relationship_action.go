package data_models

import (
	"errors"
	"time"

	"github.com/jinzhu/gorm"
)

// Constellations relationship edit history (epic #4587, subtask #4599-C1).
//
// A per-player undo/redo log over B1's relationship edit API. Every set/clear a
// player makes appends a RelationshipAction that captures the pair's state
// *before* the edit; undo replays that prior state, redo re-applies the edit.
//
// Per the operator's ruling (epic #4587 c25775) the app enforces no game rules:
// an undo is a plain UI correction of a mis-click with no special-casing of the
// Rejection clear (a back may restore any prior connection), and history is
// bounded to the newest HistoryCapPerPlayer actions per player. Undo/redo are
// caller-scoped — each player has their own stack of the edits they made — so a
// shared pair edited by two players is last-write-wins, and one player's undo
// reverts to the state that player recorded regardless of a sibling's later
// edit.

// HistoryCapPerPlayer bounds retained undoable actions per player ("maximum 20
// actions for each player"). Kept a constant so a later change is one edit.
const HistoryCapPerPlayer = 20

var (
	// ErrNothingToUndo / ErrNothingToRedo are benign empty-stack conditions the
	// handler surfaces as a no-op, not a failure.
	ErrNothingToUndo = errors.New("no action to undo")
	ErrNothingToRedo = errors.New("no action to redo")
)

// RelationshipActionKind is the edit an action records.
type RelationshipActionKind string

const (
	ActionSet   RelationshipActionKind = "set"
	ActionClear RelationshipActionKind = "clear"
)

// RelationshipAction is one undoable edit a player made to the shared graph.
// The pair is stored normalized (PairLow <= PairHigh) so an edit and its undo
// address the same unordered pair regardless of click order. The action's own
// target is (Kind, TypeID, FromUserID, ToUserID); PrevExisted + the Prev* fields
// capture what the pair looked like before, so undo can restore it.
type RelationshipAction struct {
	gorm.Model
	RoomID      uint `json:"room_id" gorm:"not null;index"`
	ActorUserID uint `json:"actor_user_id" gorm:"not null;index"`
	PairLow     uint `json:"pair_low" gorm:"not null"`
	PairHigh    uint `json:"pair_high" gorm:"not null"`

	Kind       RelationshipActionKind `json:"kind" gorm:"not null"`
	TypeID     uint                   `json:"type_id"`      // the type set (0 for a clear)
	FromUserID uint                   `json:"from_user_id"` // direction set (0 for a clear)
	ToUserID   uint                   `json:"to_user_id"`

	PrevExisted bool `json:"prev_existed"`
	PrevTypeID  uint `json:"prev_type_id"`
	PrevFrom    uint `json:"prev_from"`
	PrevTo      uint `json:"prev_to"`

	Undone bool `json:"undone" gorm:"not null;default:false;index"`
}

// TableName pins the physical table name.
func (RelationshipAction) TableName() string { return "constellation_relationship_actions" }

func normPair(a, b uint) (uint, uint) {
	if a <= b {
		return a, b
	}
	return b, a
}

// prevState is the pair's active-edge state captured before an edit.
type prevState struct {
	existed          bool
	typeID, from, to uint
}

// capturePrev reads the pair's current active edge (if any) for the history row.
func capturePrev(db *gorm.DB, roomID, a, b uint) prevState {
	if rel, ok := activePairEdge(db, roomID, a, b); ok {
		return prevState{existed: true, typeID: rel.TypeID, from: rel.FromUserID, to: rel.ToUserID}
	}
	return prevState{}
}

// recordAction truncates the actor's redo suffix (undone actions), appends the
// new action, and enforces the per-player cap. Called after a successful edit.
func recordAction(db *gorm.DB, roomID, actor uint, kind RelationshipActionKind, typeID, from, to uint, prev prevState) error {
	// A new edit invalidates the actor's redo stack.
	if err := db.Where("room_id = ? AND actor_user_id = ? AND undone = ?", roomID, actor, true).
		Delete(&RelationshipAction{}).Error; err != nil {
		return err
	}
	low, high := normPair(from, to)
	a := RelationshipAction{
		RoomID: roomID, ActorUserID: actor, PairLow: low, PairHigh: high,
		Kind: kind, TypeID: typeID, FromUserID: from, ToUserID: to,
		PrevExisted: prev.existed, PrevTypeID: prev.typeID, PrevFrom: prev.from, PrevTo: prev.to,
	}
	if err := db.Create(&a).Error; err != nil {
		return err
	}
	return enforceCap(db, roomID, actor)
}

// enforceCap keeps only the newest HistoryCapPerPlayer actions for the actor,
// dropping the oldest (which become un-undoable, the intended storage bound).
// Ids are plucked newest-first and sliced in Go rather than via SQL OFFSET,
// which SQLite rejects without a LIMIT — keeping the query portable across the
// sqlite test DB and the Postgres prod DB.
func enforceCap(db *gorm.DB, roomID, actor uint) error {
	var ids []uint
	if err := db.Model(&RelationshipAction{}).
		Where("room_id = ? AND actor_user_id = ?", roomID, actor).
		Order("id DESC").Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) <= HistoryCapPerPlayer {
		return nil
	}
	stale := ids[HistoryCapPerPlayer:]
	return db.Where("id IN (?)", stale).Delete(&RelationshipAction{}).Error
}

// deactivatePair soft-clears the pair's active edge if one exists (a no-op
// otherwise). Used by undo/redo to reach a "no active edge" state without
// logging a new action.
func deactivatePair(db *gorm.DB, roomID, a, b uint) error {
	rel, ok := activePairEdge(db, roomID, a, b)
	if !ok {
		return nil
	}
	now := time.Now()
	return db.Model(&rel).Update("cleared_at", now).Error
}

// SetRelationshipWithHistory applies a set edit and records it on the actor's
// undo stack. Thin wrapper over B1's SetRelationship — the base function stays
// history-free so its own callers and tests are unaffected.
func SetRelationshipWithHistory(db *gorm.DB, room Room, actorUserID, fromUserID, toUserID, typeID uint) (Relationship, error) {
	prev := capturePrev(db, room.ID, fromUserID, toUserID)
	rel, err := SetRelationship(db, room, fromUserID, toUserID, typeID)
	if err != nil {
		return rel, err
	}
	if err := recordAction(db, room.ID, actorUserID, ActionSet, typeID, fromUserID, toUserID, prev); err != nil {
		return rel, err
	}
	if err := recordEvent(db, room.ID, actorUserID, ActionSet, typeID, fromUserID, toUserID); err != nil {
		return rel, err
	}
	return rel, nil
}

// ClearRelationshipWithHistory applies a clear edit and records it on the
// actor's undo stack.
func ClearRelationshipWithHistory(db *gorm.DB, room Room, actorUserID, fromUserID, toUserID uint) error {
	prev := capturePrev(db, room.ID, fromUserID, toUserID)
	if err := ClearRelationship(db, room, fromUserID, toUserID); err != nil {
		return err
	}
	if err := recordAction(db, room.ID, actorUserID, ActionClear, 0, fromUserID, toUserID, prev); err != nil {
		return err
	}
	return recordEvent(db, room.ID, actorUserID, ActionClear, 0, fromUserID, toUserID)
}

// UndoRelationship reverts the actor's most recent not-yet-undone action,
// restoring the pair to the state captured before that action. Returns
// ErrNothingToUndo when the actor's undo stack is empty.
func UndoRelationship(db *gorm.DB, room Room, actorUserID uint) error {
	var a RelationshipAction
	err := db.Where("room_id = ? AND actor_user_id = ? AND undone = ?", room.ID, actorUserID, false).
		Order("id DESC").First(&a).Error
	if err != nil {
		return ErrNothingToUndo
	}
	if a.PrevExisted {
		if _, err := SetRelationship(db, room, a.PrevFrom, a.PrevTo, a.PrevTypeID); err != nil {
			return err
		}
		if err := recordEvent(db, room.ID, actorUserID, ActionSet, a.PrevTypeID, a.PrevFrom, a.PrevTo); err != nil {
			return err
		}
	} else {
		if err := deactivatePair(db, room.ID, a.PairLow, a.PairHigh); err != nil {
			return err
		}
		if err := recordEvent(db, room.ID, actorUserID, ActionClear, 0, a.PairLow, a.PairHigh); err != nil {
			return err
		}
	}
	return db.Model(&a).Update("undone", true).Error
}

// RedoRelationship re-applies the actor's earliest undone action (the most
// recently undone in timeline order). Returns ErrNothingToRedo when there is
// nothing to redo.
func RedoRelationship(db *gorm.DB, room Room, actorUserID uint) error {
	var a RelationshipAction
	err := db.Where("room_id = ? AND actor_user_id = ? AND undone = ?", room.ID, actorUserID, true).
		Order("id ASC").First(&a).Error
	if err != nil {
		return ErrNothingToRedo
	}
	if a.Kind == ActionSet {
		if _, err := SetRelationship(db, room, a.FromUserID, a.ToUserID, a.TypeID); err != nil {
			return err
		}
		if err := recordEvent(db, room.ID, actorUserID, ActionSet, a.TypeID, a.FromUserID, a.ToUserID); err != nil {
			return err
		}
	} else {
		if err := deactivatePair(db, room.ID, a.PairLow, a.PairHigh); err != nil {
			return err
		}
		if err := recordEvent(db, room.ID, actorUserID, ActionClear, 0, a.PairLow, a.PairHigh); err != nil {
			return err
		}
	}
	return db.Model(&a).Update("undone", false).Error
}

// PlayerHistory returns the actor's retained actions in timeline (oldest-first)
// order — the bounded per-player edit log the frontend reads.
func PlayerHistory(db *gorm.DB, roomID, actorUserID uint) ([]RelationshipAction, error) {
	var actions []RelationshipAction
	err := db.Where("room_id = ? AND actor_user_id = ?", roomID, actorUserID).
		Order("id ASC").Find(&actions).Error
	return actions, err
}

// RoomHistoryCursor returns a monotonic marker for the room's edit history — the
// max RelationshipAction id in the room, or 0 when none. The D1 state snapshot
// carries it so a poller can tell a new edit was recorded (and refetch the
// history panel) without diffing the log every tick. It advances on a new
// edit; undo/redo change graph state, which the snapshot's relationships list
// already reflects, so the cursor need not move for them.
func RoomHistoryCursor(db *gorm.DB, roomID uint) uint {
	var row struct{ MaxID *uint }
	db.Model(&RelationshipAction{}).
		Where("room_id = ?", roomID).
		Select("MAX(id) AS max_id").Scan(&row)
	if row.MaxID == nil {
		return 0
	}
	return *row.MaxID
}
