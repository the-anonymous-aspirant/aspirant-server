package handlers

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// AssetMapping represents a mapping between a semantic name and an asset
type AssetMapping struct {
	Hash string `json:"hash"`
	Type string `json:"type"`
}

// AssetMappingsConfig holds all asset mappings
type AssetMappingsConfig map[string]AssetMapping

const assetMappingsFile = "asset_mappings.json"

// GetAssetMappingsHandler returns the current asset mappings
func GetAssetMappingsHandler(c *gin.Context) {
	mappings, err := loadAssetMappings()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to load asset mappings")
		return
	}

	c.JSON(http.StatusOK, mappings)
}

// UpdateAssetMappingsHandler updates the asset mappings configuration
func UpdateAssetMappingsHandler(c *gin.Context) {
	var newMappings AssetMappingsConfig

	if err := c.ShouldBindJSON(&newMappings); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	// Validate the mappings
	if err := validateAssetMappings(newMappings); err != nil {
		RespondWithError(c, http.StatusBadRequest, err.Error())
		return
	}

	// Save the mappings
	if err := saveAssetMappings(newMappings); err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to save asset mappings")
		return
	}

	RespondWithSuccess(c, gin.H{"mappings": newMappings}, "Asset mappings updated successfully")
}

// loadAssetMappings loads the asset mappings from file or returns defaults
func loadAssetMappings() (AssetMappingsConfig, error) {
	// Check if file exists
	if _, err := os.Stat(assetMappingsFile); os.IsNotExist(err) {
		// Return default mappings if file doesn't exist
		return getDefaultAssetMappings(), nil
	}

	// Read file
	data, err := ioutil.ReadFile(assetMappingsFile)
	if err != nil {
		return nil, err
	}

	// Parse JSON
	var mappings AssetMappingsConfig
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, err
	}

	return mappings, nil
}

// saveAssetMappings saves the asset mappings to file
func saveAssetMappings(mappings AssetMappingsConfig) error {
	// Ensure directory exists
	dir := filepath.Dir(assetMappingsFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(mappings, "", "  ")
	if err != nil {
		return err
	}

	// Write to file
	return ioutil.WriteFile(assetMappingsFile, data, 0644)
}

// validateAssetMappings validates the asset mappings structure
func validateAssetMappings(mappings AssetMappingsConfig) error {
	validTypes := map[string]bool{
		"image":    true,
		"audio":    true,
		"video":    true,
		"document": true,
		"other":    true,
	}

	for name, mapping := range mappings {
		if name == "" {
			return gin.Error{Err: nil, Type: gin.ErrorTypeBind, Meta: "Asset name cannot be empty"}
		}

		if mapping.Hash == "" {
			return gin.Error{Err: nil, Type: gin.ErrorTypeBind, Meta: "Asset hash cannot be empty"}
		}

		if !validTypes[mapping.Type] {
			return gin.Error{Err: nil, Type: gin.ErrorTypeBind, Meta: "Invalid asset type: " + mapping.Type}
		}
	}

	return nil
}

// getDefaultAssetMappings returns the default asset mappings
func getDefaultAssetMappings() AssetMappingsConfig {
	return AssetMappingsConfig{
		// Images
		"aspiring_hand":           {Hash: "b75d0a40c2e14902f69e47f6988b0aa4", Type: "image"},
		"ludde":                   {Hash: "30d289dc6f5539e0aee0d8799c59dd02", Type: "image"},
		"emotion_tracker_icon":    {Hash: "ae541adf30941214bfeeb6109105b755", Type: "image"},
		"rbguesser_icon":          {Hash: "8ac7a4252e1d2422531a4fc47ce86ec9", Type: "image"},
		"ludde_meal_tracker_icon": {Hash: "30d289dc6f5539e0aee0d8799c59dd02", Type: "image"},
		"sql_icon":                {Hash: "74ed6ea3fc82f8f268bca4b8183e1a28", Type: "image"},
		"wordweaver_icon":         {Hash: "3c6eba6921724336db8cd9e1723609ba", Type: "image"},
		"flappyduo_icon":          {Hash: "be63c826259552d6d98c1c0fa138a71c", Type: "image"},
		"transparency_icon":       {Hash: "6f6accdcb3a9e77c7a66a41e6fb0f949", Type: "image"},
		"home_icon":               {Hash: "6a9cd5807678009a717d5f472d179876", Type: "image"},
		"default_user":            {Hash: "1802de25dec1a75d49e6bd5649d135d2", Type: "image"},
		"message_user_icon":       {Hash: "1802de25dec1a75d49e6bd5649d135d2", Type: "image"},
		"default":                 {Hash: "babd3aeb9544a9d3e623757494942d70", Type: "image"},
		"admin":                   {Hash: "7ab4892d1d4c0855d00d8e8da03bb173", Type: "image"},
		"family":                  {Hash: "23768a1dce5932b2bfadcc9277e14e1c", Type: "image"},
		"applications":            {Hash: "21ad7ec5e702e6fb91c8142bcb18ff67", Type: "image"},
		"messages":                {Hash: "1bb958fd3f0c1b839bf97a0d7f055480", Type: "image"},
		"30year_gift_icon":        {Hash: "f87358812d08ff6809f2bdd6115b66a8", Type: "image"},
		"gift-tile-1":             {Hash: "c66478d942db796f4392a1203bd47418", Type: "image"},
		"gift-tile-2":             {Hash: "b54091ffd7469d4dc70450011ef33a68", Type: "image"},
		"gift-tile-3":             {Hash: "3ba022e66246dd2a160693a9fc289e42", Type: "image"},
		"gift-tile-4":             {Hash: "353b3b7aa247975cf7477d604782ffb0", Type: "image"},
		"gift-tile-5":             {Hash: "7a48c472ff20d3d096823fa6f3dcd7bd", Type: "image"},
		"gift-tile-6":             {Hash: "cdebc9f38911a72bea5508f99dd4537e", Type: "image"},
		"gift-tile-7":             {Hash: "62b04e506ce3010ede70bc247e80bddb", Type: "image"},
		"gift-tile-8":             {Hash: "251aedf920f534f50b7aaf12ececaa6a", Type: "image"},
		"gift-tile-9":             {Hash: "796f215d76aa3e1db1fb24d6fa5032d8", Type: "image"},
		"qr-30-year-gift":         {Hash: "c21f41ffd5314e235f78fd3efe3e7464", Type: "image"},

		// Audio
		"ludde-sound":          {Hash: "93da53623e880afed235e170f55894ab", Type: "audio"},
		"game-bg-music":        {Hash: "07ba67e86c21edb47a67728cfb6aa4ad", Type: "audio"},
		"game-score-sound":     {Hash: "93da53623e880afed235e170f55894ab", Type: "audio"},
		"game-fanfare-sound":   {Hash: "60c112c8f24954a645593514ec1fdad6", Type: "audio"},
		"game-flappyduo-sound": {Hash: "d2846c0c7beaae70942256c443315912", Type: "audio"},
		"birthday-fanfare":     {Hash: "0ab465040e6198fae962940358d24f68", Type: "audio"},
	}
}
