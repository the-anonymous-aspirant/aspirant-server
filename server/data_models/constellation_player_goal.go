package data_models

import (
	"errors"

	"github.com/jinzhu/gorm"
)

// PlayerGoal is the goal card one player has selected in one room (epic #4807).
// One goal per player per room: selecting again replaces the prior choice. The
// selection is PRIVATE — it is surfaced only in the selecting player's own
// /state (see BuildRoomState), never in another player's, by the same
// serializer-scoping discipline #4809 used for edges. Privacy is a property of
// the read, not baked into storage, so the operator's "for now" (#4807 c27159)
// is a one-line change to relax.
type PlayerGoal struct {
	gorm.Model
	RoomID     uint `json:"room_id" gorm:"not null;index:idx_player_goal_room_user,unique"`
	UserID     uint `json:"user_id" gorm:"not null;index:idx_player_goal_room_user,unique"`
	GoalCardID uint `json:"goal_card_id" gorm:"not null"`
}

var (
	ErrGoalNotMember   = errors.New("only a member of the room may select a goal")
	ErrGoalUnknownCard = errors.New("unknown goal card")
)

// SetPlayerGoal records (or replaces) the caller's selected goal in the room.
// The caller must be a current member and cardID must name a real goal card.
func SetPlayerGoal(db *gorm.DB, room Room, userID, cardID uint) (PlayerGoal, error) {
	if !IsRoomMember(db, room.ID, userID) {
		return PlayerGoal{}, ErrGoalNotMember
	}
	if _, ok := GetGoalCardByID(db, cardID); !ok {
		return PlayerGoal{}, ErrGoalUnknownCard
	}

	var existing PlayerGoal
	err := db.Where("room_id = ? AND user_id = ?", room.ID, userID).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		pg := PlayerGoal{RoomID: room.ID, UserID: userID, GoalCardID: cardID}
		if err := db.Create(&pg).Error; err != nil {
			return PlayerGoal{}, err
		}
		return pg, nil
	}
	if err != nil {
		return PlayerGoal{}, err
	}
	if err := db.Model(&existing).Update("goal_card_id", cardID).Error; err != nil {
		return PlayerGoal{}, err
	}
	return existing, nil
}

// ClearPlayerGoal removes the caller's selected goal in the room, if any. It is
// idempotent: clearing when no goal is set is not an error.
func ClearPlayerGoal(db *gorm.DB, room Room, userID uint) error {
	if !IsRoomMember(db, room.ID, userID) {
		return ErrGoalNotMember
	}
	return db.Where("room_id = ? AND user_id = ?", room.ID, userID).Delete(&PlayerGoal{}).Error
}

// GetPlayerGoal returns the goal card the given player has selected in the room,
// if any.
func GetPlayerGoal(db *gorm.DB, roomID, userID uint) (GoalCard, bool) {
	var pg PlayerGoal
	if db.Where("room_id = ? AND user_id = ?", roomID, userID).First(&pg).Error != nil {
		return GoalCard{}, false
	}
	return GetGoalCardByID(db, pg.GoalCardID)
}
