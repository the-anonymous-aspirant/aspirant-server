package data_models

import "github.com/jinzhu/gorm"

type GoalComment struct {
	gorm.Model
	NodeID uint   `json:"node_id" gorm:"not null;index"`
	UserID uint   `json:"user_id" gorm:"not null;index"`
	Body   string `json:"body" gorm:"type:text;not null"`
}

func (GoalComment) TableName() string {
	return "goal_comments"
}
