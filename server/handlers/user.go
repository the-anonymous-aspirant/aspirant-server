package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// userInput is used to bind JSON from the frontend, which still sends access_role as a string.
type userInput struct {
	Username   string `json:"username"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	AccessRole string `json:"access_role"`
	Comment    string `json:"comment"`
}

// LoginUserHandler handles retrieving a user by username
func LoginUserHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	username := c.Param("username")
	if username == "" {
		RespondWithError(c, http.StatusBadRequest, "Username parameter is required")
		return
	}

	var user data_models.User
	if err := db.Preload("Role").Where("username= ?", username).First(&user).Error; err != nil {
		log.Printf("User not found: %s", username)
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	RespondWithSuccess(c, user.ToResponse(), "User retrieved successfully")
}

// GetUserHandler handles retrieving a user by ID
func GetUserHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "User ID is required")
		return
	}

	user, err := data_models.GetUserById(db, id)
	if err != nil {
		log.Printf("User not found with ID %s: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	RespondWithSuccess(c, user.ToResponse(), "User retrieved successfully")
}

// GetAllUsersHandler handles retrieving all users with pagination
func GetAllUsersHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	page, pageSize := parsePagination(c)
	offset := (page - 1) * pageSize

	var total int64
	if err := db.Model(&data_models.User{}).Count(&total).Error; err != nil {
		log.Printf("Error counting users: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving users")
		return
	}

	var users []data_models.User
	if err := db.Preload("Role").Offset(offset).Limit(pageSize).Find(&users).Error; err != nil {
		log.Printf("Error retrieving users: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving users")
		return
	}

	responses := make([]data_models.UserResponse, len(users))
	for i := range users {
		responses[i] = users[i].ToResponse()
	}

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    responses,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// CreateUserHandler handles creating a new user
func CreateUserHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	var input userInput
	if err := c.ShouldBindJSON(&input); err != nil {
		log.Printf("Invalid user data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid user data")
		return
	}

	if input.Username == "" || input.Password == "" {
		RespondWithError(c, http.StatusBadRequest, "Username and password are required")
		return
	}

	// Check if username already exists
	var existingUser data_models.User
	if err := db.Where("username = ?", input.Username).First(&existingUser).Error; err == nil {
		RespondWithError(c, http.StatusConflict, "Username already exists")
		return
	}

	// Resolve role name to ID
	roleName := input.AccessRole
	if roleName == "" {
		roleName = "User"
	}
	role, err := data_models.GetRoleByName(db, roleName)
	if err != nil {
		log.Printf("Invalid role '%s': %v", roleName, err)
		RespondWithError(c, http.StatusBadRequest, "Invalid role")
		return
	}

	user := data_models.User{
		Username: input.Username,
		Email:    input.Email,
		RoleID:   role.ID,
		Comment:  input.Comment,
	}

	if err := user.HashPassword(input.Password); err != nil {
		log.Printf("Error hashing password: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error processing user data")
		return
	}

	if err := user.CreateUser(db); err != nil {
		log.Printf("Error creating user: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating user")
		return
	}

	// Reload with role for the response
	db.Preload("Role").First(&user, user.ID)
	RespondWithSuccess(c, user.ToResponse(), "User created successfully")
}

// UpdateUserHandler handles updating a user's information
func UpdateUserHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "User ID is required")
		return
	}

	var user data_models.User
	if err := db.Preload("Role").Where("id = ?", id).First(&user).Error; err != nil {
		log.Printf("User not found with ID %s: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	currentPassword := user.Password

	var input userInput
	if err := c.ShouldBindJSON(&input); err != nil {
		log.Printf("Invalid user data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid user data")
		return
	}

	// Update fields from input
	if input.Username != "" {
		user.Username = input.Username
	}
	if input.Email != "" {
		user.Email = input.Email
	}
	user.Comment = input.Comment

	// Resolve role if provided
	if input.AccessRole != "" {
		role, err := data_models.GetRoleByName(db, input.AccessRole)
		if err != nil {
			log.Printf("Invalid role '%s': %v", input.AccessRole, err)
			RespondWithError(c, http.StatusBadRequest, "Invalid role")
			return
		}
		user.RoleID = role.ID
	}

	// Only hash password if it was changed
	if input.Password != "" && input.Password != currentPassword {
		if err := user.HashPassword(input.Password); err != nil {
			log.Printf("Error hashing password: %v", err)
			RespondWithError(c, http.StatusInternalServerError, "Error processing user data")
			return
		}
	} else {
		user.Password = currentPassword
	}

	if err := user.UpdateUser(db); err != nil {
		log.Printf("Error updating user: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error updating user")
		return
	}

	// Reload role for response
	db.Preload("Role").First(&user, user.ID)
	RespondWithSuccess(c, user.ToResponse(), "User updated successfully")
}

// DeleteUserHandler handles deleting a user
func DeleteUserHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "User ID is required")
		return
	}

	var user data_models.User
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		log.Printf("User not found with ID %s: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	// Check if the user is attempting to delete themselves
	currentUserID, exists := c.Get("user_id")
	if exists && currentUserID.(uint) == user.ID {
		RespondWithError(c, http.StatusBadRequest, "Cannot delete your own account")
		return
	}

	if err := user.DeleteUser(db); err != nil {
		log.Printf("Error deleting user: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error deleting user")
		return
	}

	RespondWithSuccess(c, nil, "User deleted successfully")
}

// BootstrapUserHandler handles creating the first admin user when no users exist
func BootstrapUserHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	// Check if any users exist
	var userCount int64
	if err := db.Model(&data_models.User{}).Count(&userCount).Error; err != nil {
		log.Printf("Error checking user count: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error checking user count")
		return
	}

	// If users exist, require authentication
	if userCount > 0 {
		RespondWithError(c, http.StatusForbidden, "Bootstrap not allowed: users already exist")
		return
	}

	var input userInput
	if err := c.ShouldBindJSON(&input); err != nil {
		log.Printf("Invalid user data: %v", err)
		RespondWithError(c, http.StatusBadRequest, "Invalid user data")
		return
	}

	if input.Username == "" || input.Password == "" {
		RespondWithError(c, http.StatusBadRequest, "Username and password are required")
		return
	}

	// Default to Admin role for bootstrap
	roleName := input.AccessRole
	if roleName == "" {
		roleName = "Admin"
	}
	role, err := data_models.GetRoleByName(db, roleName)
	if err != nil {
		log.Printf("Invalid role '%s': %v", roleName, err)
		RespondWithError(c, http.StatusInternalServerError, "Error resolving role")
		return
	}

	user := data_models.User{
		Username: input.Username,
		Email:    input.Email,
		RoleID:   role.ID,
		Comment:  input.Comment,
	}

	if err := user.HashPassword(input.Password); err != nil {
		log.Printf("Error hashing password: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error processing user data")
		return
	}

	if err := user.CreateUser(db); err != nil {
		log.Printf("Error creating bootstrap user: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error creating user")
		return
	}

	// Reload with role for response
	db.Preload("Role").First(&user, user.ID)
	log.Printf("Bootstrap admin user created: %s", user.Username)
	RespondWithSuccess(c, user.ToResponse(), "Bootstrap admin user created successfully")
}

// GetAllRolesHandler handles retrieving all roles with pagination
func GetAllRolesHandler(c *gin.Context) {
	db := c.MustGet("db").(*gorm.DB)

	page, pageSize := parsePagination(c)
	offset := (page - 1) * pageSize

	var total int64
	if err := db.Model(&data_models.Role{}).Count(&total).Error; err != nil {
		log.Printf("Error counting roles: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving roles")
		return
	}

	var roles []data_models.Role
	if err := db.Offset(offset).Limit(pageSize).Find(&roles).Error; err != nil {
		log.Printf("Error retrieving roles: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Error retrieving roles")
		return
	}

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    roles,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}
