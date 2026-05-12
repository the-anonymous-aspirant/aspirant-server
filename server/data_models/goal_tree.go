package data_models

import "github.com/jinzhu/gorm"

type GoalTree struct {
	gorm.Model
	UserID uint   `json:"user_id" gorm:"not null;index"`
	Name   string `json:"name" gorm:"type:varchar(255);not null"`
}

func (GoalTree) TableName() string {
	return "goal_trees"
}
