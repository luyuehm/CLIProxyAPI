package licenseserver

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

const (
	DefaultLeaseDuration = 24 * time.Hour
	GracePeriod          = 7 * 24 * time.Hour
)

type ActivateRequest struct {
	LicenseKey string `json:"license_key"`
	MachineID  string `json:"machine_id"`
}

type ActivateResponse struct {
	Valid          bool   `json:"valid"`
	ActivatedAt    string `json:"activated_at,omitempty"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
	Message        string `json:"message,omitempty"`
}

type HeartbeatRequest struct {
	LicenseKey string `json:"license_key"`
	MachineID  string `json:"machine_id"`
}

type HeartbeatResponse struct {
	Valid          bool   `json:"valid"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
	Message        string `json:"message,omitempty"`
}

type StatusResponse struct {
	Valid          bool   `json:"valid"`
	ActivatedAt    string `json:"activated_at,omitempty"`
	LastHeartbeat  string `json:"last_heartbeat,omitempty"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
	MachineID      string `json:"machine_id,omitempty"`
	Status         string `json:"status,omitempty"`
	Message        string `json:"message,omitempty"`
}

type Server struct {
	store         *Store
	leaseDuration time.Duration
}

func NewServer(store *Store, leaseDuration time.Duration) *Server {
	if leaseDuration <= 0 {
		leaseDuration = DefaultLeaseDuration
	}
	return &Server{store: store, leaseDuration: leaseDuration}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/activate", s.handleActivate)
	mux.HandleFunc("/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/status", s.handleStatus)
	return loggingMiddleware(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only POST allowed")
		return
	}

	var req ActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LicenseKey == "" || req.MachineID == "" {
		writeError(w, http.StatusBadRequest, "license_key and machine_id are required")
		return
	}

	record, err := s.store.Activate(req.LicenseKey, req.MachineID, s.leaseDuration)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrKeyNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}

	writeJSON(w, ActivateResponse{
		Valid:          true,
		ActivatedAt:    record.ActivatedAt,
		LeaseExpiresAt: record.LeaseExpiresAt,
		Message:        "license activated",
	})
	log.Printf("[ACTIVATE] key=%s machine=%s expires=%s", req.LicenseKey, req.MachineID, record.LeaseExpiresAt)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only POST allowed")
		return
	}

	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LicenseKey == "" || req.MachineID == "" {
		writeError(w, http.StatusBadRequest, "license_key and machine_id are required")
		return
	}

	record, err := s.store.Heartbeat(req.LicenseKey, req.MachineID, s.leaseDuration)
	if err != nil {
		status := http.StatusForbidden
		message := err.Error()
		if errors.Is(err, ErrNotActivated) {
			message = "license not activated, call /activate first"
		}
		writeError(w, status, message)
		return
	}

	writeJSON(w, HeartbeatResponse{
		Valid:          true,
		LeaseExpiresAt: record.LeaseExpiresAt,
		Message:        "heartbeat renewed",
	})
	log.Printf("[HEARTBEAT] key=%s machine=%s expires=%s", req.LicenseKey, req.MachineID, record.LeaseExpiresAt)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	licenseKey := r.URL.Query().Get("license_key")
	if licenseKey == "" {
		writeError(w, http.StatusBadRequest, "license_key query parameter is required")
		return
	}

	record, err := s.store.Status(licenseKey)
	if err != nil {
		if errors.Is(err, ErrNotActivated) {
			writeJSON(w, StatusResponse{
				Valid:   false,
				Status:  "not_activated",
				Message: "license not activated",
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	expired := false
	if record.LeaseExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, record.LeaseExpiresAt); err == nil && time.Now().After(t) {
			expired = true
		}
	}

	status := record.Status
	message := "license active"
	if expired {
		status = "expired"
		message = "license lease has expired"
	}

	writeJSON(w, StatusResponse{
		Valid:          !expired && record.Status == "active",
		ActivatedAt:    record.ActivatedAt,
		LastHeartbeat:  record.LastHeartbeat,
		LeaseExpiresAt: record.LeaseExpiresAt,
		MachineID:      record.MachineID,
		Status:         status,
		Message:        message,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	log.Printf("[ERROR] status=%d message=%s", status, message)
}

func StartServer(store *Store, addr string, leaseDuration time.Duration) error {
	svr := NewServer(store, leaseDuration)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      svr.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	log.Printf("License server starting on %s", addr)
	log.Printf("Lease duration: %v, Grace period: %v", leaseDuration, GracePeriod)
	return httpServer.ListenAndServe()
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[ACCESS] %s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}