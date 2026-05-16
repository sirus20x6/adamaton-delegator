package budget

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-core/metrics"
)

const maxBodySize = 64 * 1024 // 64KB

// Server is the HTTP server for the budget router API.
type Server struct {
	tracker  *Tracker
	router   *Router
	logger   *logrus.Logger
	mux      *mux.Router
	apiToken string
}

// NewServer creates the HTTP server and registers routes.
func NewServer(tracker *Tracker, router *Router, logger *logrus.Logger) *Server {
	s := &Server{
		tracker: tracker,
		router:  router,
		logger:  logger,
		mux:     mux.NewRouter(),
	}

	// /metrics is intentionally outside the auth subrouter and outside the
	// /api/v1 prefix so a Prometheus scraper can hit it without an API key.
	// If you need to lock /metrics down, do it at the ingress.
	s.mux.Handle("/metrics", metrics.Handler()).Methods("GET")
	s.mux.Use(metrics.Middleware("budget-router", func(r *http.Request) string {
		if route := mux.CurrentRoute(r); route != nil {
			if tmpl, err := route.GetPathTemplate(); err == nil {
				return tmpl
			}
		}
		return "unknown"
	}))

	api := s.mux.PathPrefix("/api/v1/budget").Subrouter()
	api.Use(s.authMiddleware)
	api.HandleFunc("/route", s.handleRoute).Methods("POST")
	api.HandleFunc("/report", s.handleReport).Methods("POST")
	api.HandleFunc("/status", s.handleAllStatus).Methods("GET")
	api.HandleFunc("/status/{provider}", s.handleProviderStatus).Methods("GET")
	api.HandleFunc("/history", s.handleHistory).Methods("GET")
	api.HandleFunc("/reset/{provider}", s.handleReset).Methods("POST")

	s.mux.HandleFunc("/health", s.handleHealth).Methods("GET")

	return s
}

// Start runs the HTTP server with graceful shutdown.
func (s *Server) Start(addr string) error {
	if err := s.validateListenAddress(addr); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.WithField("addr", addr).Info("Budget router listening")
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		s.logger.WithField("signal", sig).Info("Shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	case err := <-errCh:
		return err
	}
}

// SetAPIToken configures bearer/API-key authentication for budget APIs.
func (s *Server) SetAPIToken(token string) {
	s.apiToken = token
}

// --- handlers ---

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	var req RouteRequest
	if err := s.decodeBody(w, r, &req); err != nil {
		return
	}

	// Validate
	if !ValidComplexities[req.TaskComplexity] {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: fmt.Sprintf("invalid task_complexity: %q", req.TaskComplexity), Success: false,
		})
		return
	}
	if req.EstimatedTokens < 0 {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: "estimated_tokens must be non-negative", Success: false,
		})
		return
	}

	resp, err := s.router.Route(req)
	if err != nil {
		s.logger.WithError(err).Error("Route failed")
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "no providers available", Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: resp, Success: true})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req ReportRequest
	if err := s.decodeBody(w, r, &req); err != nil {
		return
	}

	// Validate
	if !ValidProviders[req.Provider] {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: fmt.Sprintf("invalid provider: %q", req.Provider), Success: false,
		})
		return
	}
	if req.TotalTokens <= 0 {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: "total_tokens must be positive", Success: false,
		})
		return
	}

	resp, err := s.tracker.Report(req)
	if err != nil {
		s.logger.WithError(err).Error("Report failed")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to record usage", Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: resp, Success: true})
}

func (s *Server) handleAllStatus(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.tracker.Status()
	if err != nil {
		s.logger.WithError(err).Error("Status failed")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to get status", Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: statuses, Success: true})
}

func (s *Server) handleProviderStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	provider := Provider(vars["provider"])

	if !ValidProviders[provider] {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: fmt.Sprintf("invalid provider: %q", provider), Success: false,
		})
		return
	}

	status, err := s.tracker.ProviderStatus(provider)
	if err != nil {
		s.logger.WithError(err).Error("ProviderStatus failed")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to get provider status", Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: status, Success: true})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	providerParam := query.Get("provider")
	provider := Provider(providerParam)
	if providerParam != "" && !ValidProviders[provider] {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: fmt.Sprintf("invalid provider: %q", providerParam), Success: false,
		})
		return
	}
	sinceStr := query.Get("since")
	limitStr := query.Get("limit")

	since := time.Now().AddDate(0, 0, -7) // default: last 7 days
	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr);
		if err != nil {
			s.sendJSON(w, http.StatusBadRequest, APIResponse{
				Error: fmt.Sprintf("invalid since timestamp (RFC3339 required): %v", err), Success: false,
			})
			return
		}
		since = t
	}

	limit := 100
	if limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil {
			s.sendJSON(w, http.StatusBadRequest, APIResponse{
				Error: fmt.Sprintf("invalid limit: %v", err), Success: false,
			})
			return
		}
		if n <= 0 || n > 1000 {
			s.sendJSON(w, http.StatusBadRequest, APIResponse{
				Error: "limit must be 1-1000", Success: false,
			})
			return
		}
		limit = n
	}

	records, err := s.tracker.History(provider, since, limit)
	if err != nil {
		s.logger.WithError(err).Error("History failed")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to get history", Success: false,
		})
		return
	}

	if records == nil {
		records = []UsageRecord{}
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: records, Success: true})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	provider := Provider(vars["provider"])

	if !ValidProviders[provider] {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: fmt.Sprintf("invalid provider: %q", provider), Success: false,
		})
		return
	}

	if err := s.tracker.ResetProvider(provider); err != nil {
		s.logger.WithError(err).Error("Reset failed")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to reset provider", Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    map[string]string{"provider": string(provider), "status": "reset"},
		Success: true,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    map[string]string{"status": "ok"},
		Success: true,
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		token := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if token == "" {
			auth := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				token = strings.TrimSpace(auth[len("Bearer "):])
			}
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.apiToken)) != 1 {
			s.sendJSON(w, http.StatusUnauthorized, APIResponse{Error: "unauthorized", Success: false})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validateListenAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	ip := net.ParseIP(host)
	if s.apiToken == "" && (host == "0.0.0.0" || host == "::" || (ip != nil && !ip.IsLoopback())) {
		return fmt.Errorf("BUDGET_API_TOKEN is required when binding budget router to %s", addr)
	}
	return nil
}

// --- helpers ---

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, dest interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: "invalid request body", Success: false,
		})
		return err
	}
	return nil
}

func (s *Server) sendJSON(w http.ResponseWriter, status int, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		s.logger.WithError(err).Error("Failed to marshal response")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		s.logger.WithError(err).Error("Failed to write response")
	}
}
