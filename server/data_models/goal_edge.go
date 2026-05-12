package data_models

import "time"

type GoalEdge struct {
	ID        uint      `json:"id" gorm:"primary_key"`
	TreeID    uint      `json:"tree_id" gorm:"not null;index"`
	FromID    uint      `json:"from_id" gorm:"not null;index"`
	ToID      uint      `json:"to_id" gorm:"not null;uniqueIndex"`
	CreatedAt time.Time `json:"created_at"`
}

func (GoalEdge) TableName() string {
	return "goal_edges"
}
