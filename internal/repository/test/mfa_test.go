package test

import (
	"path/filepath"
	"testing"
	"time"

	"cpa-usage-keeper/internal/auth"
	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/repository"
	"gorm.io/gorm"
)

func openMFARepositoryDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := repository.OpenDatabase(config.Config{SQLitePath: filepath.Join(t.TempDir(), "mfa.db")})
	if err != nil {
		t.Fatalf("OpenDatabase returned error: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestMFASecretStoreRoundTrip(t *testing.T) {
	store := repository.NewMFASecretStore(openMFARepositoryDatabase(t))

	if _, err := store.GetTOTPConfig(); err != gorm.ErrRecordNotFound {
		t.Fatalf("expected record-not-found before save, got %v", err)
	}

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret returned error: %v", err)
	}
	now := time.Now()
	if err := store.SaveTOTPConfig(auth.TOTPConfig{Secret: secret, Enabled: true, ConfirmedAt: now}); err != nil {
		t.Fatalf("SaveTOTPConfig returned error: %v", err)
	}

	loaded, err := store.GetTOTPConfig()
	if err != nil {
		t.Fatalf("GetTOTPConfig after save returned error: %v", err)
	}
	if !loaded.Enabled || loaded.Secret != secret {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}
}

func TestMFASecretStoreOverwrite(t *testing.T) {
	store := repository.NewMFASecretStore(openMFARepositoryDatabase(t))

	first, _ := auth.GenerateTOTPSecret()
	second, _ := auth.GenerateTOTPSecret()
	if err := store.SaveTOTPConfig(auth.TOTPConfig{Secret: first, Enabled: true, ConfirmedAt: time.Now()}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := store.SaveTOTPConfig(auth.TOTPConfig{Secret: second, Enabled: true, ConfirmedAt: time.Now()}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	loaded, err := store.GetTOTPConfig()
	if err != nil {
		t.Fatalf("GetTOTPConfig returned error: %v", err)
	}
	if loaded.Secret != second {
		t.Fatalf("expected overwritten secret, got %q", loaded.Secret)
	}
}
