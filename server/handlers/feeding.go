package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// GetAllFeedingTimesHandler handles retrieving all feeding times with pagination
func GetAllFeedingTimesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	page, pageSize := parsePagination(c)
	offset := (page - 1) * pageSize

	var total int64
	if err := db.Model(&data_models.LuddeFeedingTime{}).Count(&total).Error; err != nil {
		log.Printf("Error counting feeding times: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving feeding times")
		return
	}

	var feedingTimes []data_models.LuddeFeedingTime
	if err := db.Offset(offset).Limit(pageSize).Find(&feedingTimes).Error; err != nil {
		log.Printf("Error retrieving feeding times: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving feeding times")
		return
	}

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    feedingTimes,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// GetFeedingTimeHandler handles retrieving a feeding time by ID
func GetFeedingTimeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "ID parameter is required")
		return
	}

	var feedingTime data_models.LuddeFeedingTime
	if err := db.Where("id = ?", id).First(&feedingTime).Error; err != nil {
		log.Printf("Feeding time not found with ID %s: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "Feeding time not found")
		return
	}

	RespondWithSuccess(c, feedingTime, "Feeding time retrieved successfully")
}

// AddFeedingTimeHandler handles adding a new feeding time
func AddFeedingTimeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var feedingTime data_models.LuddeFeedingTime
	if err := c.ShouldBindJSON(&feedingTime); err != nil {
		log.Printf("Invalid feeding time data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid feeding time data")
		return
	}

	err := feedingTime.CreateFeedingTime(db)
	if err != nil {
		log.Printf("Error creating feeding time: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating feeding time")
		return
	}

	RespondWithSuccess(c, feedingTime, "Feeding time created successfully")
}

// DeleteFeedingTimeHandler handles deleting a feeding time
func DeleteFeedingTimeHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "ID parameter is required")
		return
	}

	var feedingTime data_models.LuddeFeedingTime
	if err := db.Where("id = ?", id).First(&feedingTime).Error; err != nil {
		log.Printf("Feeding time not found with ID %s: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "Feeding time not found")
		return
	}

	if err := feedingTime.DeleteFeedingTime(db); err != nil {
		log.Printf("Error deleting feeding time with ID %s: %v", id, err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting feeding time")
		return
	}

	RespondWithSuccess(c, nil, "Feeding time deleted successfully")
}
