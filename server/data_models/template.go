package data_models

import (
	"github.com/jinzhu/gorm"
)

// Keep a Get, GetAll, Post, Put, Delete for each new data modelt

// Define the data model here.
// gorm.model automatically adds an incrementing id, created at, updated at, and deleated at fields.
// Json sent to this struct will be mapped using the json specification comments
type Template struct {
	gorm.Model
	UniqueNotNullField      string `json:"UniqueNotNullField" gorm:"unique;not null"`
	KeepFromGetREquestField string `json:"KeepFromGetREquestField,omitempty"`
	OtherField              string `json:"OtherField"`
}

// Get a saved instance of the data model from the db by searching using the ID.
func Get(db *gorm.DB, id string) (*Template, error) {
	var template Template
	err := db.Where("id = ?", id).First(&template).Error
	if err != nil {
		return nil, err
	}
	return &template, nil
}

// By default, the gorm db infers what table is being used
// With db.Find without any more parameters, all of the data of the data model will be bound to the template and then returned
func GetAll(db *gorm.DB) ([]Template, error) {
	var templates []Template
	err := db.Find(&templates).Error

	if err != nil {
		return nil, err
	}
	return templates, nil
}

// Create a new entry based on the data model. Nothing is returned, it operates directly on the database
func (t *Template) Post(db *gorm.DB) error {
	return db.Create(t).Error
}

// Update a new entry based on the data model. Nothing is returned, it operates directly on the database
func (t *Template) Put(db *gorm.DB) error {
	return db.Save(t).Error
}

// Delete a new entry from the datamodel. Nothing is retunred, it operates directly on the database.
func (t *User) Delete(db *gorm.DB) error {
	return db.Delete(t).Error
}
