package repository

import (
	"errors"
	"fmt"
	"strings"

	"cpa-usage-keeper/internal/entities"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var (
	ErrUserNotFound    = errors.New("user not found")
	ErrUserConflict    = errors.New("username or email already exists")
	ErrInvalidCred     = errors.New("invalid credentials")
)

type UserCreateRequest struct {
	Username string         `json:"username" binding:"required"`
	Email    string         `json:"email" binding:"required"`
	Password string         `json:"password" binding:"required,min=6"`
	Role     entities.Role  `json:"role" binding:"required"`
	Quota    int64          `json:"quota"`
}

type UserUpdateRequest struct {
	Email    *string        `json:"email,omitempty"`
	Password *string        `json:"password,omitempty"`
	Role     *entities.Role `json:"role,omitempty"`
	Quota    *int64         `json:"quota,omitempty"`
	Active   *bool          `json:"active,omitempty"`
}

// UserRepository 提供基于 GORM 的用户 CRUD 操作。
type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

// SeedAdmin 在无任何用户时创建默认管理员，保证首次启动即有可登录账号。
func (r *UserRepository) SeedAdmin(username, password, email string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("user repository is not initialized")
	}
	var count int64
	if err := r.db.Model(&entities.User{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user := &entities.User{
		ID:           uuid.New().String(),
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		Role:         entities.RoleAdmin,
		APIKey:       uuid.New().String(),
		Quota:        -1,
		Active:       true,
	}
	if err := r.db.Create(user).Error; err != nil {
		return err
	}
	return nil
}

func (r *UserRepository) Create(req UserCreateRequest) (*entities.User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("user repository is not initialized")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	user := &entities.User{
		ID:           uuid.New().String(),
		Username:     req.Username,
		Email:        req.Email,
		PasswordHash: string(hash),
		Role:         req.Role,
		APIKey:       uuid.New().String(),
		Quota:        req.Quota,
		Active:       true,
	}
	if err := r.db.Create(user).Error; err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrUserConflict
		}
		return nil, err
	}
	return user, nil
}

func (r *UserRepository) GetByID(id string) (*entities.User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("user repository is not initialized")
	}
	var user entities.User
	if err := r.db.First(&user, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) GetByAPIKey(apiKey string) (*entities.User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("user repository is not initialized")
	}
	var user entities.User
	if err := r.db.First(&user, "api_key = ?", apiKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &user, nil
}

func (r *UserRepository) List() ([]entities.User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("user repository is not initialized")
	}
	var users []entities.User
	if err := r.db.Order("created_at DESC").Find(&users).Error; err != nil {
		return nil, err
	}
	for i := range users {
		users[i].APIKey = maskAPIKey(users[i].APIKey)
	}
	return users, nil
}

func (r *UserRepository) Update(id string, req UserUpdateRequest) (*entities.User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("user repository is not initialized")
	}
	user, err := r.GetByID(id)
	if err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}
	if req.Email != nil && *req.Email != user.Email {
		updates["email"] = *req.Email
	}
	if req.Role != nil && *req.Role != user.Role {
		updates["role"] = string(*req.Role)
	}
	if req.Quota != nil && *req.Quota != user.Quota {
		updates["quota"] = *req.Quota
	}
	if req.Active != nil && *req.Active != user.Active {
		updates["active"] = *req.Active
	}
	if req.Password != nil && *req.Password != "" {
		hash, hashErr := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if hashErr != nil {
			return nil, hashErr
		}
		updates["password_hash"] = string(hash)
	}
	if len(updates) == 0 {
		return user, nil
	}

	if err := r.db.Model(&entities.User{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrUserConflict
		}
		return nil, err
	}
	return r.GetByID(id)
}

func (r *UserRepository) Delete(id string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("user repository is not initialized")
	}
	result := r.db.Where("id = ?", id).Delete(&entities.User{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (r *UserRepository) Authenticate(username, password string) (*entities.User, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("user repository is not initialized")
	}
	var user entities.User
	if err := r.db.First(&user, "username = ?", username).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCred
		}
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCred
	}
	return &user, nil
}

func (r *UserRepository) IncrementUsed(apiKey string, amount int64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("user repository is not initialized")
	}
	return r.db.Model(&entities.User{}).Where("api_key = ?", apiKey).
		UpdateColumn("used", gorm.Expr("used + ?", amount)).Error
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "duplicate key")
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:4] + "****" + key[len(key)-4:]
}