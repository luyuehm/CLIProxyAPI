package api

import (
	"errors"
	"net/http"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"
	"github.com/gin-gonic/gin"
)

// userAPIHandler 提供用户管理与 RBAC 认证接口（admin/operator/viewer 角色）。
type userAPIHandler struct {
	users service.UserProvider
}

func NewUserAPIHandler(users service.UserProvider) *userAPIHandler {
	return &userAPIHandler{users: users}
}

// registerUserRoutes 注册用户管理相关路由。
func (h *userAPIHandler) registerRoutes(router gin.IRouter, authRequired gin.HandlerFunc, requireRole func(...entities.Role) gin.HandlerFunc) {
	// 公开：登录
	router.POST("/users/auth/login", h.login)

	authed := router.Group("")
	authed.Use(authRequired)
	{
		authed.GET("/users/me", h.getMe)

		admin := authed.Group("/users")
		admin.Use(requireRole(entities.RoleAdmin))
		{
			admin.GET("", h.listUsers)
			admin.GET("/:id", h.getUser)
			admin.POST("", h.createUser)
			admin.PUT("/:id", h.updateUser)
			admin.DELETE("/:id", h.deleteUser)
		}
	}
}

func (h *userAPIHandler) login(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	user, err := h.users.AuthenticateUser(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if !user.Active {
		c.JSON(http.StatusForbidden, gin.H{"error": "account is disabled"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user": user,
	})
}

func (h *userAPIHandler) getMe(c *gin.Context) {
	userID, ok := c.Get("auth_user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	idStr, ok := userID.(string)
	if !ok || idStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	user, err := h.users.GetUserByID(c.Request.Context(), idStr)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *userAPIHandler) listUsers(c *gin.Context) {
	users, err := h.users.ListUsers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
		return
	}
	c.JSON(http.StatusOK, users)
}

func (h *userAPIHandler) getUser(c *gin.Context) {
	user, err := h.users.GetUserByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *userAPIHandler) createUser(c *gin.Context) {
	var req repository.UserCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if !entities.IsValidRole(string(req.Role)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role, must be one of: admin, operator, viewer"})
		return
	}
	user, err := h.users.CreateUser(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, repository.ErrUserConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "username or email already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
		return
	}
	c.JSON(http.StatusCreated, user)
}

func (h *userAPIHandler) updateUser(c *gin.Context) {
	var req repository.UserUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Role != nil && !entities.IsValidRole(string(*req.Role)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role, must be one of: admin, operator, viewer"})
		return
	}
	user, err := h.users.UpdateUser(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if errors.Is(err, repository.ErrUserConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "username or email already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user"})
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *userAPIHandler) deleteUser(c *gin.Context) {
	userID, ok := c.Get("auth_user_id")
	if ok {
		if idStr, ok := userID.(string); ok && c.Param("id") == idStr {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete yourself"})
			return
		}
	}
	if err := h.users.DeleteUser(c.Request.Context(), c.Param("id")); err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}