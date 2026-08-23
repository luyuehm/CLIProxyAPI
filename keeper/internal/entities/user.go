package entities

import "time"

type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

func ValidRoles() []Role {
	return []Role{RoleAdmin, RoleOperator, RoleViewer}
}

func IsValidRole(r string) bool {
	for _, v := range ValidRoles() {
		if string(v) == r {
			return true
		}
	}
	return false
}

type User struct {
	ID           string    `gorm:"primaryKey;size:36" json:"id"`
	Username     string    `gorm:"uniqueIndex;size:64;not null" json:"username"`
	Email        string    `gorm:"uniqueIndex;size:128;not null" json:"email"`
	PasswordHash string    `gorm:"size:255;not null" json:"-"`
	Role         Role      `gorm:"size:20;not null;default:viewer" json:"role"`
	APIKey       string    `gorm:"uniqueIndex;size:64;not null" json:"api_key,omitempty"`
	Quota        int64     `gorm:"not null;default:0" json:"quota"`
	Used         int64     `gorm:"not null;default:0" json:"used"`
	Active       bool      `gorm:"not null;default:true" json:"active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}