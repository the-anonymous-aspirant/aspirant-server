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
	if actorUserID != fromUserID && actorUserID != toUserID {
		return Relationship{}, ErrRelNotEndpoint
	}
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
	if actorUserID != fromUserID && actorUserID != toUserID {
		return ErrRelNotEndpoint
	}
	prev := capturePrev(db, room.ID, fromUserID, toUserID)
	if err := ClearRelationship(db, room, fromUserID, toUserID); err != nil {
		return err
	}
	if err := recordAction(db, room.ID, actorUserID, ActionClear, 0, fromUserID, toUserID, prev); err != nil {
		return err
	}
	return recordEvent(db, room.ID, actorUserID, ActionClear, 0, fromUserID, toUserID)
}

// HistoryStep is what one undo or redo request did.
type HistoryStep struct {
	// Applied is true when a step was replayed onto the graph.
	Applied bool
	// Skipped counts the steps passed over on the way because the other
	// player of their pair is no longer in the room.
	Skipped int
}

// otherPlayerGone reports whether the action's other endpoint is no longer a
// current member of the room: they left, or their account was deleted and the
// seat went with it (system_3 #11384).
func otherPlayerGone(db *gorm.DB, roomID, actorUserID uint, a RelationshipAction) bool {
	other := a.PairLow
	if other == actorUserID {
		other = a.PairHigh
	}
	return !IsRoomMember(db, roomID, other)
}

// UndoRelationship reverts the actor's most recent not-yet-undone action,
// restoring the pair to the state captured before that action. Returns
// ErrNothingToUndo when the actor's undo stack is empty.
//
// A step that would put an edge back to a player who is no longer in the room
// cannot apply: SetRelationship refuses a pair with a non-member. Such a step
// is marked undone and passed over, and the same call carries on with the next
// one (system_3 #11432, the operator's rule (a) in c70520). Before this the
// call returned the refusal and left the step in place, so every later undo
// met the same step and the player's older history was out of reach; another
// player could cause that on purpose by leaving. A skipped step changes no
// edge, so it writes no room-history event. Only the OTHER player's absence is
// skipped: with the actor gone, SetRelationship's refusal is returned as
// before and the stack is left alone.
func UndoRelationship(db *gorm.DB, room Room, actorUserID uint) (HistoryStep, error) {
	var step HistoryStep
	for {
		var a RelationshipAction
		err := db.Where("room_id = ? AND actor_user_id = ? AND undone = ?", room.ID, actorUserID, false).
			Order("id DESC").First(&a).Error
		if err != nil {
			if step.Skipped > 0 {
				// Every step left was skipped: nothing was applied, and that
				// is the answer, not an empty-stack no-op.
				return step, nil
			}
			return step, ErrNothingToUndo
		}
		// Write authorization (#4834): the actor may only re-write an edge they are
		// an endpoint of. New actions can only be recorded party-edges (guarded in
		// Set/ClearRelationshipWithHistory), but a grandfathered cross-party action
		// recorded before the rule must not be replayable by a non-endpoint.
		if actorUserID != a.PairLow && actorUserID != a.PairHigh {
			return step, ErrRelNotEndpoint
		}
		if a.PrevExisted {
			if otherPlayerGone(db, room.ID, actorUserID, a) {
				if err := db.Model(&a).Update("undone", true).Error; err != nil {
					return step, err
				}
				step.Skipped++
				continue
			}
			if _, err := SetRelationship(db, room, a.PrevFrom, a.PrevTo, a.PrevTypeID); err != nil {
				return step, err
			}
			if err := recordEvent(db, room.ID, actorUserID, ActionSet, a.PrevTypeID, a.PrevFrom, a.PrevTo); err != nil {
				return step, err
			}
		} else {
			if err := deactivatePair(db, room.ID, a.PairLow, a.PairHigh); err != nil {
				return step, err
			}
			if err := recordEvent(db, room.ID, actorUserID, ActionClear, 0, a.PairLow, a.PairHigh); err != nil {
				return step, err
			}
		}
		if err := db.Model(&a).Update("undone", true).Error; err != nil {
			return step, err
		}
		step.Applied = true
		return step, nil
	}
}

// RedoRelationship re-applies the actor's earliest undone action (the most
// recently undone in timeline order). Returns ErrNothingToRedo when there is
// nothing to redo. A set whose other player is no longer in the room is marked
// done and passed over, as in UndoRelationship (system_3 #11432).
func RedoRelationship(db *gorm.DB, room Room, actorUserID uint) (HistoryStep, error) {
	var step HistoryStep
	for {
		var a RelationshipAction
		err := db.Where("room_id = ? AND actor_user_id = ? AND undone = ?", room.ID, actorUserID, true).
			Order("id ASC").First(&a).Error
		if err != nil {
			if step.Skipped > 0 {
				return step, nil
			}
			return step, ErrNothingToRedo
		}
		// Write authorization (#4834): as in undo, a non-endpoint may not replay a
		// grandfathered cross-party action.
		if actorUserID != a.PairLow && actorUserID != a.PairHigh {
			return step, ErrRelNotEndpoint
		}
		if a.Kind == ActionSet {
			if otherPlayerGone(db, room.ID, actorUserID, a) {
				if err := db.Model(&a).Update("undone", false).Error; err != nil {
					return step, err
				}
				step.Skipped++
				continue
			}
			if _, err := SetRelationship(db, room, a.FromUserID, a.ToUserID, a.TypeID); err != nil {
				return step, err
			}
			if err := recordEvent(db, room.ID, actorUserID, ActionSet, a.TypeID, a.FromUserID, a.ToUserID); err != nil {
				return step, err
			}
		} else {
			if err := deactivatePair(db, room.ID, a.PairLow, a.PairHigh); err != nil {
				return step, err
			}
			if err := recordEvent(db, room.ID, actorUserID, ActionClear, 0, a.PairLow, a.PairHigh); err != nil {
				return step, err
			}
		}
		if err := db.Model(&a).Update("undone", false).Error; err != nil {
			return step, err
		}
		step.Applied = true
		return step, nil
	}
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
