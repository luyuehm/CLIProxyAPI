package auth

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// UserClaims 是用户 JWT 的载荷结构。
type UserClaims struct {
	UserID   string         `json:"user_id"`
	Username string         `json:"username"`
	Role     entities.Role   `json:"role"`
	jwt.RegisteredClaims
}

// UserJWTAuth 提供用户管理与 RBAC 接口的 JWT 认证中间件。
type UserJWTAuth struct {
	secret []byte
	expiry time.Duration
	users  *repository.UserRepository
}

func NewUserJWTAuth(secret string, expiry time.Duration, users *repository.UserRepository) *UserJWTAuth {
	return &UserJWTAuth{
		secret: []byte(secret),
		expiry: expiry,
		users:  users,
	}
}

// GenerateToken 为指定用户签发 JWT。
func (a *UserJWTAuth) GenerateToken(user *entities.User) (string, error) {
	claims := &UserClaims{
		UserID:   user.ID,
		Username: user.Username,
		Role:     user.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(a.expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   user.ID,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(a.secret)
}

// RequireAuth 返回要求携带有效 JWT 的中间件。
func (a *UserJWTAuth) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := extractBearerToken(c)
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization token"})
			return
		}

		claims := &UserClaims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return a.secret, nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set("auth_user_id", claims.UserID)
		c.Set("auth_username", claims.Username)
		c.Set("auth_role", claims.Role)
		c.Next()
	}
}

// RequireRole 返回要求当前用户属于指定角色之一的中间件。
func (a *UserJWTAuth) RequireRole(roles ...entities.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		roleValue, exists := c.Get("auth_role")
		if !exists {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access denied"})
			return
		}
		userRole, ok := roleValue.(entities.Role)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access denied"})
			return
		}
		for _, r := range roles {
			if userRole == r {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
	}
}

// APIKeyAuth 返回按 X-API-Key 头认证用户的中间件。
func (a *UserJWTAuth) APIKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-API-Key")
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing X-API-Key header"})
			return
		}
		user, err := a.users.GetByAPIKey(apiKey)
		if err != nil || !user.Active {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid or inactive API key"})
			return
		}
		c.Set("auth_user_id", user.ID)
		c.Set("auth_username", user.Username)
		c.Set("auth_role", user.Role)
		c.Set("api_user", user)
		c.Next()
	}
}

func extractBearerToken(c *gin.Context) string {
	if header := c.GetHeader("Authorization"); header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return parts[1]
		}
		return parts[0]
	}
	return ""
}