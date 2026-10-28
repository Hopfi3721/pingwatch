// Package api — HTTP-слой pingwatch: JSON API и встроенный дашборд.
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
	"github.com/Hopfi3721/pingwatch/internal/storage"
)

const (
	maxBodyBytes     = 64 << 10
	defaultListLimit = 50
	maxListLimit     = 500
)

//go:embed web/index.html
var webFS embed.FS

type Server struct {
	store  storage.Store
	logger *log.Logger
	now    func() time.Time
}

func New(store storage.Store, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}

	return &Server{
		store:  store,
		logger: logger,
		now:    time.Now,
	}
}

// Handler собирает маршруты и оборачивает их middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/targets", s.handleListTargets)
	mux.HandleFunc("POST /api/targets", s.handleCreateTarget)
	mux.HandleFunc("GET /api/targets/{id}", s.handleGetTarget)
	mux.HandleFunc("DELETE /api/targets/{id}", s.handleDeleteTarget)
	mux.HandleFunc("GET /api/targets/{id}/results", s.handleListResults)
	mux.HandleFunc("GET /api/targets/{id}/stats", s.handleStats)
	mux.HandleFunc("GET /", s.handleDashboard)

	return s.recoverPanic(s.logRequests(mux))
}

type targetView struct {
	monitor.Target
	Status         monitor.Status `json:"status"`
	LastCheckedAt  *time.Time     `json:"last_checked_at,omitempty"`
	LastStatusCode int            `json:"last_status_code,omitempty"`
	LastLatencyMS  int64          `json:"last_latency_ms,omitempty"`
	LastError      string         `json:"last_error,omitempty"`
}

type listResponse struct {
	Items []targetView `json:"items"`
}

type resultsResponse struct {
	Items []monitor.Result `json:"items"`
	Limit int              `json:"limit"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type createTargetRequest struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	IntervalSeconds int    `json:"interval_seconds"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	Enabled         *bool  `json:"enabled"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.ListTargets(r.Context())
	if err != nil {
		s.internalError(w, "list targets", err)
		return
	}

	latest, err := s.store.LatestResults(r.Context())
	if err != nil {
		s.internalError(w, "latest results", err)
		return
	}

	items := make([]targetView, 0, len(targets))
	for _, target := range targets {
		view := targetView{Target: target, Status: monitor.StatusUnknown}
		if result, ok := latest[target.ID]; ok {
			view = buildView(target, &result)
		}
		items = append(items, view)
	}

	writeJSON(w, http.StatusOK, listResponse{Items: items})
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var req createTargetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	target := monitor.Target{
		Name:            req.Name,
		URL:             req.URL,
		IntervalSeconds: req.IntervalSeconds,
		TimeoutSeconds:  req.TimeoutSeconds,
		Enabled:         enabled,
		CreatedAt:       s.now().UTC(),
	}
	target.Normalize()

	if err := target.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.CreateTarget(r.Context(), &target); err != nil {
		s.internalError(w, "create target", err)
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/api/targets/%d", target.ID))
	writeJSON(w, http.StatusCreated, buildView(target, nil))
}

func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	id, err := parseTargetID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	target, err := s.store.GetTarget(r.Context(), id)
	if errors.Is(err, monitor.ErrTargetNotFound) {
		writeError(w, http.StatusNotFound, "target not found")
		return
	}
	if err != nil {
		s.internalError(w, "get target", err)
		return
	}

	result, err := s.store.LatestResult(r.Context(), id)
	if err != nil && !errors.Is(err, monitor.ErrResultNotFound) {
		s.internalError(w, "latest result", err)
		return
	}

	writeJSON(w, http.StatusOK, buildView(*target, result))
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id, err := parseTargetID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	err = s.store.DeleteTarget(r.Context(), id)
	if errors.Is(err, monitor.ErrTargetNotFound) {
		writeError(w, http.StatusNotFound, "target not found")
		return
	}
	if err != nil {
		s.internalError(w, "delete target", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListResults(w http.ResponseWriter, r *http.Request) {
	id, err := parseTargetID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := s.store.GetTarget(r.Context(), id); errors.Is(err, monitor.ErrTargetNotFound) {
		writeError(w, http.StatusNotFound, "target not found")
		return
	} else if err != nil {
		s.internalError(w, "get target", err)
		return
	}

	limit := defaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > maxListLimit {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be an integer between 1 and %d", maxListLimit))
			return
		}
		limit = value
	}

	results, err := s.store.ListResults(r.Context(), id, limit)
	if err != nil {
		s.internalError(w, "list results", err)
		return
	}

	writeJSON(w, http.StatusOK, resultsResponse{Items: results, Limit: limit})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	id, err := parseTargetID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := s.store.GetTarget(r.Context(), id); errors.Is(err, monitor.ErrTargetNotFound) {
		writeError(w, http.StatusNotFound, "target not found")
		return
	} else if err != nil {
		s.internalError(w, "get target", err)
		return
	}

	stats, err := s.store.Stats(r.Context(), id)
	if err != nil {
		s.internalError(w, "stats", err)
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		s.internalError(w, "read dashboard", err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func buildView(target monitor.Target, result *monitor.Result) targetView {
	view := targetView{Target: target, Status: monitor.StatusUnknown}
	if result == nil {
		return view
	}

	checkedAt := result.CheckedAt
	view.Status = result.Status
	view.LastCheckedAt = &checkedAt
	view.LastStatusCode = result.StatusCode
	view.LastLatencyMS = result.LatencyMS
	view.LastError = result.Error

	return view
}

func parseTargetID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("id must be a positive integer")
	}

	return id, nil
}

var errUnsupportedMediaType = errors.New("content type must be application/json")

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return errUnsupportedMediaType
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return err
	}

	// Второй объект в теле — почти всегда ошибка клиента: молча игнорировать
	// его нельзя, иначе часть запроса "теряется" без объяснений.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON object")
	}

	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnsupportedMediaType) {
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}

	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}

	writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
}

func (s *Server) internalError(w http.ResponseWriter, operation string, err error) {
	s.logger.Printf("api: %s: %v", operation, err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Заголовки уже отправлены, клиенту помочь нечем.
		log.Printf("api: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Printf("%s %s %d %s", r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Printf("api: panic on %s %s: %v\n%s", r.Method, r.URL.Path, recovered, debug.Stack())
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}
