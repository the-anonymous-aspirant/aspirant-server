package data_models

import "time"

type GoalTree struct {
	ID               uint       `json:"id" gorm:"primary_key"`
	UserID           uint       `json:"user_id" gorm:"not null;index"`
	Name             string     `json:"name" gorm:"type:varchar(255);not null"`
	RootNodeID       *uint      `json:"root_node_id"`
	EditingSessionID *string    `json:"editing_session_id" gorm:"type:varchar(64)"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty" gorm:"index"`
}

func (GoalTree) TableName() string {
	return "goal_trees"
}
