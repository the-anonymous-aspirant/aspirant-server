package data_models

import (
	"github.com/jinzhu/gorm"
	"time"
)

// Message represents a message entity in the system.
// It includes the content of the message, the ID of the sender, and the time the message was sent.
// The gorm.Model embedded struct includes fields ID, CreatedAt, UpdatedAt, and DeletedAt to provide
// basic model functionalities such as auto-incrementing primary key, timestamps, and soft delete support.
type Message struct {
	gorm.Model
	Content  string    `json:"Content"`
	SenderID uint      `json:"SenderID"`
	SentAt   time.Time `json:"SentAt"`
}

// Create a new message
func (m *Message) Create(db *gorm.DB) error {
	return db.Create(m).Error
}

// GetAllMessages retrieves all messages from the database
func GetAllMessages(db *gorm.DB) ([]Message, error) {
	var messages []Message
	err := db.Order("updated_at desc").Find(&messages).Error
	if err != nil {
		return nil, err
	}
	return messages, nil
}

// Read a message by ID
func GetMessageByID(db *gorm.DB, id uint) (*Message, error) {
	var m Message
	err := db.First(&m, id).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Update a message
func (m *Message) Update(db *gorm.DB) error {
	return db.Save(m).Error
}

// Delete a message
func (m *Message) Delete(db *gorm.DB) error {
	return db.Delete(m).Error
}
