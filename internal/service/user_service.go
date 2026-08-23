package service

import (
	"context"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
	"gorm.io/gorm"
)

type UserProvider interface {
	CreateUser(ctx context.Context, req repository.UserCreateRequest) (*entities.User, error)
	GetUserByID(ctx context.Context, id string) (*entities.User, error)
	ListUsers(ctx context.Context) ([]entities.User, error)
	UpdateUser(ctx context.Context, id string, req repository.UserUpdateRequest) (*entities.User, error)
	DeleteUser(ctx context.Context, id string) error
	AuthenticateUser(ctx context.Context, username, password string) (*entities.User, error)
	SeedAdmin(ctx context.Context, username, password, email string) error
}

type userService struct {
	repo *repository.UserRepository
}

func NewUserService(db *gorm.DB) UserProvider {
	return &userService{repo: repository.NewUserRepository(db)}
}

func (s *userService) CreateUser(_ context.Context, req repository.UserCreateRequest) (*entities.User, error) {
	return s.repo.Create(req)
}

func (s *userService) GetUserByID(_ context.Context, id string) (*entities.User, error) {
	return s.repo.GetByID(id)
}

func (s *userService) ListUsers(_ context.Context) ([]entities.User, error) {
	return s.repo.List()
}

func (s *userService) UpdateUser(_ context.Context, id string, req repository.UserUpdateRequest) (*entities.User, error) {
	return s.repo.Update(id, req)
}

func (s *userService) DeleteUser(_ context.Context, id string) error {
	return s.repo.Delete(id)
}

func (s *userService) AuthenticateUser(_ context.Context, username, password string) (*entities.User, error) {
	return s.repo.Authenticate(username, password)
}

func (s *userService) SeedAdmin(_ context.Context, username, password, email string) error {
	return s.repo.SeedAdmin(username, password, email)
}