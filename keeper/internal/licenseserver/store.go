package licenseserver

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

var (
	ErrKeyNotFound    = errors.New("license key not found")
	ErrKeyExpired     = errors.New("license key has expired")
	ErrMaxActivations = errors.New("max activations reached")
	ErrNotActivated   = errors.New("license not activated")
)

type LicenseKeyDef struct {
	MaxActivations int               `json:"max_activations"`
	ExpiresAt      string            `json:"expires_at"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type ActivationRecord struct {
	ActivatedAt    string `json:"activated_at"`
	LastHeartbeat  string `json:"last_heartbeat"`
	LeaseExpiresAt string `json:"lease_expires_at"`
	MachineID      string `json:"machine_id"`
	Status         string `json:"status"`
}

// stateFileData is the on-disk JSON format for the state file.
type stateFileData struct {
	Activations map[string]map[string]ActivationRecord `json:"activations"`
}

type Store struct {
	mu       sync.RWMutex
	keysFile string
	stateFile string
	// Keys is the license key definitions (key name -> def).
	Keys map[string]LicenseKeyDef `json:"keys"`
	// Activations is key name -> machine ID -> activation record.
	Activations map[string]map[string]ActivationRecord
}

func NewStore(keysFile, stateFile string) (*Store, error) {
	s := &Store{
		keysFile:    keysFile,
		stateFile:   stateFile,
		Keys:        map[string]LicenseKeyDef{},
		Activations: map[string]map[string]ActivationRecord{},
	}
	if err := s.loadKeys(); err != nil {
		return nil, err
	}
	_ = s.loadState()
	return s, nil
}

func (s *Store) loadKeys() error {
	data, err := os.ReadFile(s.keysFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var raw struct {
		Keys map[string]LicenseKeyDef `json:"keys"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Keys != nil {
		s.Keys = raw.Keys
	}
	return nil
}

func (s *Store) loadState() error {
	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if s.Activations == nil {
				s.Activations = map[string]map[string]ActivationRecord{}
			}
			return nil
		}
		return err
	}
	var fileData stateFileData
	if err := json.Unmarshal(data, &fileData); err != nil {
		return err
	}
	if s.Activations == nil {
		s.Activations = map[string]map[string]ActivationRecord{}
	}
	for key, machines := range fileData.Activations {
		if s.Activations[key] == nil {
			s.Activations[key] = map[string]ActivationRecord{}
		}
		for mid, rec := range machines {
			s.Activations[key][mid] = rec
		}
	}
	return nil
}

func (s *Store) saveState() error {
	s.mu.RLock()
	fileData := stateFileData{Activations: s.Activations}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(fileData, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.stateFile, data, 0644)
}

func (s *Store) ValidateKey(key string) (LicenseKeyDef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	def, ok := s.Keys[key]
	if !ok {
		return LicenseKeyDef{}, ErrKeyNotFound
	}
	if def.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, def.ExpiresAt)
		if err == nil && t.Before(time.Now()) {
			return LicenseKeyDef{}, ErrKeyExpired
		}
	}
	return def, nil
}

func (s *Store) Activate(key, machineID string, leaseDuration time.Duration) (ActivationRecord, error) {
	def, err := s.ValidateKey(key)
	if err != nil {
		return ActivationRecord{}, err
	}

	record := ActivationRecord{
		ActivatedAt:    time.Now().UTC().Format(time.RFC3339),
		LastHeartbeat:  time.Now().UTC().Format(time.RFC3339),
		LeaseExpiresAt: time.Now().UTC().Add(leaseDuration).Format(time.RFC3339),
		MachineID:      machineID,
		Status:         "active",
	}

	s.mu.Lock()
	if s.Activations[key] == nil {
		s.Activations[key] = map[string]ActivationRecord{}
	} else if def.MaxActivations > 0 {
		// Check if this machine is already activated — if so, reuse the slot.
		if _, exists := s.Activations[key][machineID]; !exists {
			if len(s.Activations[key]) >= def.MaxActivations {
				s.mu.Unlock()
				return ActivationRecord{}, ErrMaxActivations
			}
		}
	}
	s.Activations[key][machineID] = record
	s.mu.Unlock()

	if err := s.saveState(); err != nil {
		return ActivationRecord{}, err
	}
	return record, nil
}

func (s *Store) Heartbeat(key, machineID string, leaseDuration time.Duration) (ActivationRecord, error) {
	s.mu.Lock()
	machines, ok := s.Activations[key]
	if !ok {
		s.mu.Unlock()
		return ActivationRecord{}, ErrNotActivated
	}
	rec, exists := machines[machineID]
	if !exists || rec.Status != "active" {
		s.mu.Unlock()
		return ActivationRecord{}, ErrNotActivated
	}

	now := time.Now().UTC()
	rec.LastHeartbeat = now.Format(time.RFC3339)
	rec.LeaseExpiresAt = now.Add(leaseDuration).Format(time.RFC3339)
	s.Activations[key][machineID] = rec
	s.mu.Unlock()

	if err := s.saveState(); err != nil {
		return ActivationRecord{}, err
	}
	return rec, nil
}

func (s *Store) Status(key string) (ActivationRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	machines, ok := s.Activations[key]
	if !ok || len(machines) == 0 {
		return ActivationRecord{}, ErrNotActivated
	}
	// Return the most recent activation for this key.
	var latest ActivationRecord
	for _, rec := range machines {
		if rec.ActivatedAt > latest.ActivatedAt {
			latest = rec
		}
	}
	return latest, nil
}

func (s *Store) MachineStatus(key, machineID string) (ActivationRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	machines, ok := s.Activations[key]
	if !ok {
		return ActivationRecord{}, ErrNotActivated
	}
	rec, exists := machines[machineID]
	if !exists {
		return ActivationRecord{}, ErrNotActivated
	}
	return rec, nil
}