package handlers

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

const filesBasePath = "/data/files"

const maxStoragePerUser int64 = 50 * 1024 * 1024 * 1024 // 50 GB
const maxStorageShared int64 = 50 * 1024 * 1024 * 1024  // 50 GB

// FileInfo represents metadata about a stored file or directory
type FileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

// validateFilename rejects path traversal attempts
func validateFilename(filename string) error {
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return fmt.Errorf("invalid filename")
	}
	if filename == "" {
		return fmt.Errorf("filename is required")
	}
	return nil
}

// validateSubpath rejects path traversal attempts in subfolder paths
func validateSubpath(subpath string) error {
	if subpath == "" {
		return nil
	}
	cleaned := filepath.Clean(subpath)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return fmt.Errorf("invalid path")
	}
	if strings.Contains(subpath, "\\") {
		return fmt.Errorf("invalid path")
	}
	return nil
}

// listFiles reads a directory and returns file and directory metadata
func listFiles(dirPath string) ([]FileInfo, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []FileInfo{}, nil
		}
		return nil, err
	}

	dirs := []FileInfo{}
	files := []FileInfo{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fi := FileInfo{
			Name:    info.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			IsDir:   entry.IsDir(),
		}
		if entry.IsDir() {
			fi.Size = 0
			dirs = append(dirs, fi)
		} else {
			files = append(files, fi)
		}
	}

	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	return append(dirs, files...), nil
}

// getUserDir returns the file storage path for a specific user
func getUserDir(userID uint) string {
	return filepath.Join(filesBasePath, "users", fmt.Sprintf("%d", userID))
}

// getSharedDir returns the shared file storage path
func getSharedDir() string {
	return filepath.Join(filesBasePath, "shared")
}

// ListFilesHandler lists files in the authenticated user's private folder
func ListFilesHandler(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	dirPath := filepath.Join(getUserDir(userID.(uint)), subpath)
	files, err := listFiles(dirPath)
	if err != nil {
		log.Printf("Error listing files for user %d: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list files")
		return
	}

	RespondWithSuccess(c, files, "Files listed successfully")
}

// ListSharedFilesHandler lists files in the shared folder
func ListSharedFilesHandler(c *gin.Context) {
	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	dirPath := filepath.Join(getSharedDir(), subpath)
	files, err := listFiles(dirPath)
	if err != nil {
		log.Printf("Error listing shared files: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list shared files")
		return
	}

	RespondWithSuccess(c, files, "Shared files listed successfully")
}

// UploadFileHandler uploads a file to the authenticated user's private folder
func UploadFileHandler(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "No file provided")
		return
	}

	if err := validateFilename(file.Filename); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid filename")
		return
	}

	baseDir := getUserDir(userID.(uint))
	currentSize, _, err := dirSize(baseDir)
	if err != nil {
		log.Printf("Error calculating storage for user %d: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to check storage usage")
		return
	}
	if currentSize+file.Size > maxStoragePerUser {
		RespondWithError(c, http.StatusBadRequest, "Storage limit exceeded (50 GB)")
		return
	}

	dirPath := filepath.Join(baseDir, subpath)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		log.Printf("Error creating directory %s: %v", dirPath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create storage directory")
		return
	}

	destPath := filepath.Join(dirPath, file.Filename)
	if err := c.SaveUploadedFile(file, destPath); err != nil {
		log.Printf("Error saving file %s: %v", destPath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to save file")
		return
	}

	log.Printf("User %d uploaded file: %s", userID, file.Filename)
	RespondWithSuccess(c, nil, "File uploaded successfully")
}

// UploadSharedFileHandler uploads a file to the shared folder
func UploadSharedFileHandler(c *gin.Context) {
	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "No file provided")
		return
	}

	if err := validateFilename(file.Filename); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid filename")
		return
	}

	baseDir := getSharedDir()
	currentSize, _, err := dirSize(baseDir)
	if err != nil {
		log.Printf("Error calculating shared storage: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to check storage usage")
		return
	}
	if currentSize+file.Size > maxStorageShared {
		RespondWithError(c, http.StatusBadRequest, "Storage limit exceeded (50 GB)")
		return
	}

	dirPath := filepath.Join(baseDir, subpath)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		log.Printf("Error creating shared directory: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create storage directory")
		return
	}

	destPath := filepath.Join(dirPath, file.Filename)
	if err := c.SaveUploadedFile(file, destPath); err != nil {
		log.Printf("Error saving shared file %s: %v", destPath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to save file")
		return
	}

	userID, _ := c.Get("user_id")
	log.Printf("User %d uploaded shared file: %s", userID, file.Filename)
	RespondWithSuccess(c, nil, "File uploaded to shared folder successfully")
}

// DownloadFileHandler serves a file from the authenticated user's private folder
func DownloadFileHandler(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	filename := c.Param("filename")
	if err := validateFilename(filename); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid filename")
		return
	}

	filePath := filepath.Join(getUserDir(userID.(uint)), subpath, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		RespondWithError(c, http.StatusNotFound, "File not found")
		return
	}

	c.File(filePath)
}

// DownloadSharedFileHandler serves a file from the shared folder
func DownloadSharedFileHandler(c *gin.Context) {
	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	filename := c.Param("filename")
	if err := validateFilename(filename); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid filename")
		return
	}

	filePath := filepath.Join(getSharedDir(), subpath, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		RespondWithError(c, http.StatusNotFound, "File not found")
		return
	}

	c.File(filePath)
}

// DeleteFileHandler deletes a file or empty directory from the authenticated user's private folder
func DeleteFileHandler(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	filename := c.Param("filename")
	if err := validateFilename(filename); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid filename")
		return
	}

	filePath := filepath.Join(getUserDir(userID.(uint)), subpath, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		RespondWithError(c, http.StatusNotFound, "File not found")
		return
	}

	if err := os.Remove(filePath); err != nil {
		log.Printf("Error deleting %s: %v", filePath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete (directory may not be empty)")
		return
	}

	log.Printf("User %d deleted: %s", userID, filepath.Join(subpath, filename))
	RespondWithSuccess(c, nil, "Deleted successfully")
}

// CreateFolderRequest is the JSON body for folder creation
type CreateFolderRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// CreateFolderHandler creates a subfolder in the authenticated user's private folder
func CreateFolderHandler(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	var req CreateFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := validateFilename(req.Name); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid folder name")
		return
	}
	if err := validateSubpath(req.Path); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	dirPath := filepath.Join(getUserDir(userID.(uint)), req.Path, req.Name)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		log.Printf("Error creating folder %s: %v", dirPath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create folder")
		return
	}

	log.Printf("User %d created folder: %s", userID, filepath.Join(req.Path, req.Name))
	RespondWithSuccess(c, nil, "Folder created successfully")
}

// CreateSharedFolderHandler creates a subfolder in the shared folder
func CreateSharedFolderHandler(c *gin.Context) {
	var req CreateFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := validateFilename(req.Name); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid folder name")
		return
	}
	if err := validateSubpath(req.Path); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	dirPath := filepath.Join(getSharedDir(), req.Path, req.Name)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		log.Printf("Error creating shared folder %s: %v", dirPath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create folder")
		return
	}

	userID, _ := c.Get("user_id")
	log.Printf("User %d created shared folder: %s", userID, filepath.Join(req.Path, req.Name))
	RespondWithSuccess(c, nil, "Shared folder created successfully")
}

// DeleteSharedFileHandler deletes a file or empty directory from the shared folder (admin only)
func DeleteSharedFileHandler(c *gin.Context) {
	subpath := c.Query("path")
	if err := validateSubpath(subpath); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid path")
		return
	}

	filename := c.Param("filename")
	if err := validateFilename(filename); err != nil {
		RespondWithError(c, http.StatusBadRequest, "Invalid filename")
		return
	}

	filePath := filepath.Join(getSharedDir(), subpath, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		RespondWithError(c, http.StatusNotFound, "File not found")
		return
	}

	if err := os.Remove(filePath); err != nil {
		log.Printf("Error deleting shared file %s: %v", filePath, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete (directory may not be empty)")
		return
	}

	userID, _ := c.Get("user_id")
	log.Printf("Admin %d deleted shared file: %s", userID, filepath.Join(subpath, filename))
	RespondWithSuccess(c, nil, "Deleted successfully")
}

// FolderUsage represents storage usage for a single folder
type FolderUsage struct {
	Name      string `json:"name"`
	FileCount int    `json:"file_count"`
	TotalSize int64  `json:"total_size"`
}

// dirSize recursively calculates the total size and file count of a directory
func dirSize(path string) (int64, int, error) {
	var totalSize int64
	var fileCount int

	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip entries we can't read
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err == nil {
				totalSize += info.Size()
				fileCount++
			}
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	return totalSize, fileCount, nil
}

// StorageUsageHandler returns disk usage per user and for the shared folder (admin only)
func StorageUsageHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	// Build a map of user ID -> username
	users, err := data_models.GetAllUsers(db)
	if err != nil {
		log.Printf("Error fetching users for storage usage: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to fetch users")
		return
	}
	nameByID := make(map[string]string)
	for _, u := range users {
		nameByID[fmt.Sprintf("%d", u.ID)] = u.Username
	}

	// Shared folder usage
	sharedSize, sharedCount, err := dirSize(getSharedDir())
	if err != nil {
		log.Printf("Error reading shared dir size: %v", err)
	}

	result := []FolderUsage{
		{Name: "Shared", FileCount: sharedCount, TotalSize: sharedSize},
	}

	// Per-user folder usage
	usersDir := filepath.Join(filesBasePath, "users")
	entries, err := os.ReadDir(usersDir)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error reading users dir: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		folderID := entry.Name()
		size, count, err := dirSize(filepath.Join(usersDir, folderID))
		if err != nil {
			continue
		}

		displayName := folderID
		if name, ok := nameByID[folderID]; ok {
			displayName = name + " (ID " + folderID + ")"
		}

		// Skip empty folders
		if count == 0 {
			continue
		}

		result = append(result, FolderUsage{
			Name:      displayName,
			FileCount: count,
			TotalSize: size,
		})
	}

	// Compute grand total
	var grandTotal int64
	var grandCount int
	for _, f := range result {
		grandTotal += f.TotalSize
		grandCount += f.FileCount
	}

	RespondWithSuccess(c, gin.H{
		"folders":      result,
		"total_size":   grandTotal,
		"total_files":  grandCount,
		"max_per_user": maxStoragePerUser,
		"max_shared":   maxStorageShared,
	}, "Storage usage retrieved")
}

// OwnStorageUsageHandler returns the authenticated user's own storage usage
func OwnStorageUsageHandler(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		RespondWithError(c, http.StatusUnauthorized, "User not authenticated")
		return
	}

	size, count, err := dirSize(getUserDir(userID.(uint)))
	if err != nil {
		log.Printf("Error reading user %d dir size: %v", userID, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read storage usage")
		return
	}

	RespondWithSuccess(c, gin.H{
		"file_count": count,
		"total_size": size,
		"max_size":   maxStoragePerUser,
	}, "Storage usage retrieved")
}
