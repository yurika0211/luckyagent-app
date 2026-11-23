package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	perfDiagnosticsDBName           = "perf.db"
	perfDiagnosticsRetention        = 7 * 24 * time.Hour
	perfDiagnosticsBodyLimit        = 16 << 20
	perfDiagnosticsRetentionDaysEnv = "LUCKYAGENT_PERF_DIAGNOSTICS_RETENTION_DAYS"
)

type perfDiagnosticsUpload struct {
	CaptureID     string `json:"capture_id"`
	Sequence      int64  `json:"sequence"`
	EndpointHash  string `json:"endpoint_hash"`
	SessionHash   string `json:"session_hash"`
	AppVersion    string `json:"app_version"`
	DeviceModel   string `json:"device_model"`
	ScreenClass   string `json:"screen_class"`
	StartedAt     string `json:"started_at"`
	EndedAt       string `json:"ended_at"`
	SampleCount   int64  `json:"sample_count"`
	PayloadBase64 string `json:"payload_base64"`
	PayloadSHA256 string `json:"payload_sha256"`
}

type perfDiagnosticsUploadResponse struct {
	CaptureID string `json:"capture_id"`
	Sequence  int64  `json:"sequence"`
	Status    string `json:"status"`
}

type perfDiagnosticsStore struct {
	db        *sql.DB
	path      string
	retention time.Duration
}

func configuredPerfDiagnosticsRetention() time.Duration {
	raw := strings.TrimSpace(os.Getenv(perfDiagnosticsRetentionDaysEnv))
	if raw == "" {
		return perfDiagnosticsRetention
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > 3650 {
		return perfDiagnosticsRetention
	}
	return time.Duration(days) * 24 * time.Hour
}

func openPerfDiagnosticsStore(homeDir string) (*perfDiagnosticsStore, error) {
	homeDir = strings.TrimSpace(homeDir)
	if homeDir == "" {
		return nil, errors.New("luckyagent home is empty")
	}
	dir := filepath.Join(homeDir, "diagnostics")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostics directory: %w", err)
	}
	path := filepath.Join(dir, perfDiagnosticsDBName)
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open diagnostics sqlite: %w", err)
	}
	store := &perfDiagnosticsStore{db: db, path: path, retention: configuredPerfDiagnosticsRetention()}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *perfDiagnosticsStore) initialize() error {
	if s == nil || s.db == nil {
		return errors.New("diagnostics store is nil")
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS captures (
    capture_id TEXT PRIMARY KEY,
    endpoint_hash TEXT NOT NULL,
    session_hash TEXT NOT NULL,
    app_version TEXT NOT NULL,
    device_model TEXT NOT NULL,
    screen_class TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT NOT NULL,
    sample_count INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS batches (
    capture_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    payload BLOB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY (capture_id, sequence),
    FOREIGN KEY (capture_id) REFERENCES captures(capture_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS batches_received_at_idx ON batches(received_at);
CREATE INDEX IF NOT EXISTS captures_expires_at_idx ON captures(expires_at);
`)
	if err != nil {
		return fmt.Errorf("initialize diagnostics sqlite: %w", err)
	}
	return nil
}

func (s *perfDiagnosticsStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *perfDiagnosticsStore) cleanupExpired(now time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("diagnostics store is nil")
	}
	result, err := s.db.Exec(`DELETE FROM captures WHERE expires_at <= ?`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("cleanup diagnostics: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count diagnostics cleanup: %w", err)
	}
	return count, nil
}

func (s *perfDiagnosticsStore) putBatch(req perfDiagnosticsUpload, now time.Time) (status string, err error) {
	if err := validatePerfDiagnosticsUpload(req); err != nil {
		return "", err
	}
	payload, err := base64.StdEncoding.DecodeString(req.PayloadBase64)
	if err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	actualHash := hex.EncodeToString(digest[:])
	if !strings.EqualFold(actualHash, strings.TrimSpace(req.PayloadSHA256)) {
		return "", errors.New("payload sha256 mismatch")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin diagnostics transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	created := now.UTC().Format(time.RFC3339Nano)
	expires := now.Add(s.retention).UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`
INSERT INTO captures (
    capture_id, endpoint_hash, session_hash, app_version, device_model,
    screen_class, started_at, ended_at, sample_count, created_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(capture_id) DO NOTHING`,
		req.CaptureID, req.EndpointHash, req.SessionHash, req.AppVersion,
		req.DeviceModel, req.ScreenClass, req.StartedAt, req.EndedAt,
		req.SampleCount, created, expires,
	)
	if err != nil {
		return "", fmt.Errorf("insert diagnostics capture: %w", err)
	}

	var existingHash string
	rowErr := tx.QueryRow(`SELECT payload_sha256 FROM batches WHERE capture_id = ? AND sequence = ?`, req.CaptureID, req.Sequence).Scan(&existingHash)
	switch {
	case errors.Is(rowErr, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO batches (capture_id, sequence, payload, payload_sha256, received_at) VALUES (?, ?, ?, ?, ?)`,
			req.CaptureID, req.Sequence, payload, actualHash, created)
		if err != nil {
			return "", fmt.Errorf("insert diagnostics batch: %w", err)
		}
		status = "accepted"
	case rowErr != nil:
		return "", fmt.Errorf("read diagnostics batch: %w", rowErr)
	case !strings.EqualFold(existingHash, actualHash):
		return "", errors.New("diagnostics batch already exists with a different payload")
	default:
		status = "duplicate"
	}
	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("commit diagnostics batch: %w", err)
	}
	return status, nil
}

func validatePerfDiagnosticsUpload(req perfDiagnosticsUpload) error {
	if strings.TrimSpace(req.CaptureID) == "" || len(req.CaptureID) > 160 {
		return errors.New("capture_id is required")
	}
	if req.Sequence < 0 {
		return errors.New("sequence must be non-negative")
	}
	if strings.TrimSpace(req.EndpointHash) == "" || strings.TrimSpace(req.SessionHash) == "" {
		return errors.New("endpoint_hash and session_hash are required")
	}
	if len(req.PayloadBase64) == 0 {
		return errors.New("payload_base64 is required")
	}
	if len(req.PayloadBase64) > perfDiagnosticsBodyLimit {
		return errors.New("diagnostics payload is too large")
	}
	if len(strings.TrimSpace(req.PayloadSHA256)) != sha256.Size*2 {
		return errors.New("payload_sha256 is required")
	}
	return nil
}

func (s *Server) handlePerfDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.sendError(w, "method not allowed", http.StatusMethodNotAllowed, "")
		return
	}
	if s.perfDiagnostics == nil {
		s.sendError(w, "performance diagnostics storage unavailable", http.StatusServiceUnavailable, "")
		return
	}
	defer r.Body.Close()
	reqContext := r.Clone(r.Context())
	reqContext.Body = http.MaxBytesReader(w, r.Body, perfDiagnosticsBodyLimit)
	var req perfDiagnosticsUpload
	decoder := json.NewDecoder(reqContext.Body)
	if err := decoder.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			s.sendError(w, "diagnostics payload is empty", http.StatusBadRequest, "")
			return
		}
		s.sendError(w, "invalid diagnostics payload", http.StatusBadRequest, err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			s.sendError(w, "diagnostics payload has trailing data", http.StatusBadRequest, "")
		} else {
			s.sendError(w, "invalid diagnostics payload", http.StatusBadRequest, err.Error())
		}
		return
	}

	cleaned, err := s.perfDiagnostics.cleanupExpired(time.Now())
	if err != nil {
		perfLogger.Warn("performance diagnostics cleanup failed", "error", err)
	} else if cleaned > 0 {
		perfLogger.Info("performance diagnostics expired records removed", "captures", cleaned)
	}
	status, err := s.perfDiagnostics.putBatch(req, time.Now())
	if err != nil {
		perfLogger.Warn("performance diagnostics batch rejected", "capture_id", req.CaptureID, "sequence", req.Sequence, "error", err)
		statusCode := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists with a different") {
			statusCode = http.StatusConflict
		}
		s.sendError(w, "performance diagnostics rejected", statusCode, err.Error())
		return
	}
	perfLogger.Info("performance diagnostics batch stored", "capture_id", req.CaptureID, "sequence", req.Sequence, "status", status)
	s.sendJSON(w, http.StatusOK, perfDiagnosticsUploadResponse{CaptureID: req.CaptureID, Sequence: req.Sequence, Status: status})
}

// Keep this logger local to the package while allowing tests to replace it
// without coupling the handler to the application's global logger.
var perfLogger = slog.Default()
