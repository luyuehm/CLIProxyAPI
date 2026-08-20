package license

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// readCachedInfo loads the last known license info from disk.
func readCachedInfo(path string) *LicenseInfo {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var info LicenseInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil
	}
	return &info
}

// writeCachedInfo persists license info for offline fallback.
func writeCachedInfo(path string, info *LicenseInfo) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
