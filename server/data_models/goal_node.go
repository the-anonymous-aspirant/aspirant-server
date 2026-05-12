package data_models

import (
	"time"

	"github.com/jinzhu/gorm"
)

type GoalNode struct {
	gorm.Model
	TreeID       uint       `json:"tree_id" gorm:"not null;index"`
	Name         string     `json:"name" gorm:"type:varchar(255);not null"`
	NodeType     string     `json:"node_type" gorm:"type:varchar(30);not null;index"`
	Color        string     `json:"color" gorm:"type:varchar(30)"`
	Body         string     `json:"body" gorm:"type:text"`
	PlannedStart *time.Time `json:"planned_start"`
	PlannedEnd   *time.Time `json:"planned_end"`
	CompletedAt  *time.Time `json:"completed_at"`
}

func (GoalNode) TableName() string {
	return "goal_nodes"
}
