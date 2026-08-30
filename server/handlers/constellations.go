package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// GetConstellationRelationshipTypesHandler returns the Constellations
// relationship-type vocabulary (P/D/F+/F/A/R) with the colour stored per type,
// so the frontend renders each term in its own colour rather than hard-coding
// it (epic #4587, subtask #4594-A2).
func GetConstellationRelationshipTypesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	types, err := data_models.GetRelationshipTypes(db)
	if err != nil {
		log.Printf("Error retrieving relationship types: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving relationship types")
		return
	}

	c.JSON(http.StatusOK, gin.H{"relationship_types": types})
}
