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

// callerIsAdmin returns true when the request carries an Admin role
// in its JWT context (populated by AuthMiddleware). Any other value
// — including a missing key — counts as non-Admin.
func callerIsAdmin(c *gin.Context) bool {
	role, exists := c.Get("role")
	if !exists {
		return false
	}
	roleStr, ok := role.(string)
	return ok && roleStr == "Admin"
}

// GetUserHandler handles retrieving a user by ID. Admin sees the
// full UserResponse (with email/comment); every other authenticated
// caller gets the PII-stripped PublicUserResponse ({ID, username}).
//
// The item route deliberately mirrors the collection route
// (GetAllUsersHandler): both hand a non-Admin only {ID, username}, and
// neither the earlier 403-on-cross-id nor a per-id gate would add real
// protection while the collection lists the same fields in bulk — the
// 403 read as a boundary that wasn't there (security-finding #3093).
// Email and comment stay Admin-only via the DTO split (#1380); that is
// the boundary that actually matters and it is preserved here.
func GetUserHandler(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		RespondWithError(c, http.StatusBadRequest, "User ID is required")
		return
	}

	db := c.MustGet("db").(*gorm.DB)
	user, err := data_models.GetUserById(db, id)
	if err != nil {
		log.Printf("User not found with ID %s: %v", id, err)
		RespondWithError(c, http.StatusNotFound, "User not found")
		return
	}

	// #4223 item 4: stamp the current display name (DTO methods have no DB
	// access); fall back to the username when the temporal row is absent.
	display := data_models.CurrentDisplayName(db, user.ID)
	if display == "" {
		display = user.Username
	}
	if callerIsAdmin(c) {
		dto := user.ToResponse()
		dto.DisplayName = display
		RespondWithSuccess(c, dto, "User retrieved successfully")
	} else {
		dto := user.ToPublicResponse()
		dto.DisplayName = display
		RespondWithSuccess(c, dto, "User retrieved successfully")
	}
}

// GetAllUsersHandler handles retrieving all users with pagination.
// Admin callers see the full UserResponse in Items; non-Admin
// authenticated callers see PublicUserResponse ({ID, username}) — the
// message board's mapping of author_id → username still works while
// email and comment stay behind an Admin gate (#1380) and access_role
// is no longer enumerable by a non-Admin (#3093).
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

	// #4223 item 4: stamp the current display name onto each DTO (the DTO
	// methods have no DB access). Batch-resolve once to avoid an N+1, and fall
	// back to the username for any user the temporal table has yet to backfill.
	ids := make([]uint, len(users))
	for i := range users {
		ids[i] = users[i].ID
	}
	displayNames := data_models.CurrentDisplayNames(db, ids)
	nameFor := func(u data_models.User) string {
		if n, ok := displayNames[u.ID]; ok && n != "" {
			return n
		}
		return u.Username
	}

	if callerIsAdmin(c) {
		responses := make([]data_models.UserResponse, len(users))
		for i := range users {
			responses[i] = users[i].ToResponse()
			responses[i].DisplayName = nameFor(users[i])
		}
		c.JSON(http.StatusOK, PaginatedResponse{
			Items:    responses,
			Total:    total,
			Page:     page,
			PageSize: pageSize,
		})
		return
	}

	public := make([]data_models.PublicUserResponse, len(users))
	for i := range users {
		public[i] = users[i].ToPublicResponse()
		public[i].DisplayName = nameFor(users[i])
	}
	c.JSON(http.StatusOK, PaginatedResponse{
		Items:    public,
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

	// Resolve role name to ID. Default to the Viewer tier (#5113-A2/D4) — the
	// legacy "User" role no longer exists after the tier migration.
	roleName := input.AccessRole
	if roleName == "" {
		roleName = "Viewer"
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
	// An admin entered this address, so there is no self-service claim to
	// test; without the stamp the account is created unable to ever log in
	// (LoginHandler refuses an unverified account, #5113-C1).
	user.MarkEmailVerifiedNow()

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
	passwordChanged := input.Password != "" && input.Password != currentPassword
	if passwordChanged {
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

	// A password change here is an admin resetting someone's credential, which
	// is one of the two reasons to do it: the account is compromised, or the
	// person has lost access. Both want the old sessions gone (#5224). Any
	// other edit through this endpoint — a role change, a comment — leaves
	// sessions alone, because signing someone out for a typo'd comment would be
	// its own small bug.
	if passwordChanged {
		if err := data_models.RevokeSessions(db, user.ID); err != nil {
			log.Printf("ERROR: password changed for user %d but revoking its sessions failed: %v", user.ID, err)
		}
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
	// Bootstrap runs only on an empty database — there is nobody to send a
	// verification mail to, and the person running it owns the deployment.
	// Without the stamp this endpoint creates an admin who can never log in.
	user.MarkEmailVerifiedNow()

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
