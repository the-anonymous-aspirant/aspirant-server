package data_models

import (
	"errors"
	"time"

	"github.com/jinzhu/gorm"
)

// Constellations relationship-graph edges (epic #4587, subtask #4596-B1).
//
// A Relationship is one connection between two players in a room — the shared
// "Connections sheet" edge a group agrees on. This layer owns the edit API
// (set / clear / list); the graph render (F1), control panel (F2), history
// (C1), and summary prose (F4) build on this contract.
//
// Symmetry (the "rulebook symmetry" of the B1 brief, resolved against the
// operator's no-game-rules ruling — epic c25747/c25775): at most one ACTIVE
// (un-cleared) relationship exists per unordered pair of members, regardless of
// the order the two avatars were clicked. FromUserID / ToUserID preserve the
// direction the edge was last set in, so the summary renderer can say "Victor
// rejected Jimmy" — but a pair never carries two active edges. The app enforces
// no game rules beyond this one-edge-per-pair integrity constraint.
//
// Members are referenced by the player's user_id within the room (the identity
// A1's room-state DTO exposes and the frontend holds when it clicks an avatar),
// not the ephemeral RoomMember row id.

var (
	ErrRelSameMember      = errors.New("a relationship needs two distinct members")
	ErrRelNotMember       = errors.New("both endpoints must be current members of the room")
	ErrRelInvalidType     = errors.New("unknown relationship type")
	ErrRelNoActiveEdge    = errors.New("no active relationship for that pair")
	ErrRelCallerNotMember = errors.New("only a member of the room may edit its graph")
)

// Relationship is one edge in a room's shared graph.
type Relationship struct {
	gorm.Model
	RoomID     uint       `json:"room_id" gorm:"not null;index"`
	FromUserID uint       `json:"from_user_id" gorm:"not null;index"`
	ToUserID   uint       `json:"to_user_id" gorm:"not null;index"`
	TypeID     uint       `json:"type_id" gorm:"not null"`
	ClearedAt  *time.Time `json:"cleared_at"`
}

// TableName pins the physical table name.
func (Relationship) TableName() string { return "constellation_relationships" }

// IsRoomMember reports whether userID is currently in-room for roomID.
func IsRoomMember(db *gorm.DB, roomID, userID uint) bool {
	var n int
	db.Model(&RoomMember{}).
		Where("room_id = ? AND user_id = ? AND left_at IS NULL", roomID, userID).
		Count(&n)
	return n > 0
}

// relationshipTypeExists reports whether typeID names a seeded RelationshipType.
func relationshipTypeExists(db *gorm.DB, typeID uint) bool {
	var n int
	db.Model(&RelationshipType{}).Where("id = ?", typeID).Count(&n)
	return n > 0
}

// activePairEdge returns the active (un-cleared) edge for the unordered pair
// {a, b} in roomID, and whether one exists.
func activePairEdge(db *gorm.DB, roomID, a, b uint) (Relationship, bool) {
	var rel Relationship
	err := db.Where(
		"room_id = ? AND cleared_at IS NULL AND "+
			"((from_user_id = ? AND to_user_id = ?) OR (from_user_id = ? AND to_user_id = ?))",
		roomID, a, b, b, a,
	).First(&rel).Error
	if err != nil {
		return Relationship{}, false
	}
	return rel, true
}

// SetRelationship upserts the edge between two members: if the pair already has
// an active edge (in either direction) it is updated to the new type and
// direction; otherwise a new edge is created. Enforces one active edge per
// unordered pair. Validates the room is active, both endpoints are distinct
// current members, and the type is valid.
func SetRelationship(db *gorm.DB, room Room, fromUserID, toUserID, typeID uint) (Relationship, error) {
	if fromUserID == toUserID {
		return Relationship{}, ErrRelSameMember
	}
	if !IsRoomMember(db, room.ID, fromUserID) || !IsRoomMember(db, room.ID, toUserID) {
		return Relationship{}, ErrRelNotMember
	}
	if !relationshipTypeExists(db, typeID) {
		return Relationship{}, ErrRelInvalidType
	}

	if rel, ok := activePairEdge(db, room.ID, fromUserID, toUserID); ok {
		// Update in place — new type and (latest-wins) direction.
		updates := map[string]interface{}{
			"from_user_id": fromUserID,
			"to_user_id":   toUserID,
			"type_id":      typeID,
		}
		if err := db.Model(&rel).Updates(updates).Error; err != nil {
			return Relationship{}, err
		}
		return rel, nil
	}

	rel := Relationship{RoomID: room.ID, FromUserID: fromUserID, ToUserID: toUserID, TypeID: typeID}
	if err := db.Create(&rel).Error; err != nil {
		return Relationship{}, err
	}
	return rel, nil
}

// ClearRelationship soft-clears the active edge for the pair (sets cleared_at),
// keeping the row so C1's history can read prior states. Returns ErrRelNoActiveEdge
// when the pair has no active edge.
func ClearRelationship(db *gorm.DB, room Room, fromUserID, toUserID uint) error {
	if fromUserID == toUserID {
		return ErrRelSameMember
	}
	rel, ok := activePairEdge(db, room.ID, fromUserID, toUserID)
	if !ok {
		return ErrRelNoActiveEdge
	}
	now := time.Now()
	return db.Model(&rel).Update("cleared_at", now).Error
}

// RoomRelationships returns the room's active (un-cleared) edges — the shared
// graph — ordered oldest-first for a stable render.
func RoomRelationships(db *gorm.DB, roomID uint) ([]Relationship, error) {
	var rels []Relationship
	err := db.Where("room_id = ? AND cleared_at IS NULL", roomID).Order("id ASC").Find(&rels).Error
	return rels, err
}
