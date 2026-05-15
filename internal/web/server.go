package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"

	"github.com/deux2k5/dcs-ais-traffic/internal/config"
	"github.com/deux2k5/dcs-ais-traffic/internal/coordinator"
	"github.com/deux2k5/dcs-ais-traffic/internal/dcscomm"
	"github.com/deux2k5/dcs-ais-traffic/internal/updater"
)

//go:embed static
var staticFS embed.FS

// StatusResponse is returned by GET /api/status.
type StatusResponse struct {
	Enabled        bool           `json:"enabled"`
	Theatre        string         `json:"theatre"`
	ShipCount      int            `json:"shipCount"`
	SpawnedCount   int            `json:"spawnedCount"`
	PendingCount   int            `json:"pendingCount"`
	ModelsLoaded   int            `json:"modelsLoaded"`
	AISConnected   bool           `json:"aisConnected"`
	HookConnected  bool           `json:"hookConnected"`
	Categories     map[string]int `json:"categories"`
}

// ToggleRequest is the body of POST /api/toggle.
type ToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// Server is the HTTP server providing the REST API and static dashboard.
type Server struct {
	cfg   *config.Config
	coord *coordinator.Coordinator
	dcs   *dcscomm.Server
}

// NewServer creates a web server.
func NewServer(cfg *config.Config, coord *coordinator.Coordinator, dcs *dcscomm.Server) *Server {
	return &Server{cfg: cfg, coord: coord, dcs: dcs}
}

// Start begins serving HTTP. This blocks.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// API routes.
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/toggle", s.handleToggle)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/ships", s.handleShips)
	mux.HandleFunc("/api/filters", s.handleFilters)
	mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("/api/update/apply", s.handleUpdateApply)

	// Static files.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	s.cfg.RLock()
	port := s.cfg.Web.Port
	s.cfg.RUnlock()

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[WEB] server listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.cfg.RLock()
	enabled := s.cfg.AIS.Enabled
	s.cfg.RUnlock()

	stats := s.coord.Stats()

	resp := StatusResponse{
		Enabled:        enabled,
		Theatre:        s.coord.Theatre(),
		ShipCount:      stats.Total,
		SpawnedCount:   stats.Spawned,
		PendingCount:   stats.Pending,
		ModelsLoaded:   stats.ModelsLoaded,
		AISConnected:   s.coord.AISClient().IsConnected(),
		HookConnected:  s.dcs.IsConnected(),
		Categories:     stats.Categories,
	}

	writeJSON(w, resp)
}

func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.coord.Toggle(req.Enabled)

	writeJSON(w, map[string]bool{"enabled": req.Enabled})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snap := s.cfg.Snapshot()
		// Redact API key from GET responses — never expose it over the network.
		snap.AIS.APIKey = ""
		writeJSON(w, snap)

	case http.MethodPost:
		var incoming config.ConfigData
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		keyChanged := false
		s.cfg.Lock()
		if incoming.AIS.APIKey != "" && incoming.AIS.APIKey != s.cfg.AIS.APIKey {
			s.cfg.AIS.APIKey = incoming.AIS.APIKey
			keyChanged = true
		}
		if incoming.AIS.MaxShips > 0 {
			s.cfg.AIS.MaxShips = incoming.AIS.MaxShips
		}
		if incoming.AIS.UpdateSeconds > 0 {
			s.cfg.AIS.UpdateSeconds = incoming.AIS.UpdateSeconds
		}
		if incoming.AIS.StaleMinutes > 0 {
			s.cfg.AIS.StaleMinutes = incoming.AIS.StaleMinutes
		}
		s.cfg.Unlock()

		if err := s.cfg.Save(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// If the API key changed and AIS is enabled, restart the feed.
		if keyChanged {
			s.coord.RestartAIS()
		}

		snap := s.cfg.Snapshot()
		snap.AIS.APIKey = ""
		writeJSON(w, snap)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleShips(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ships := s.coord.Ships()
	writeJSON(w, ships)
}

func (s *Server) handleFilters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var filters config.FilterConfig
	if err := json.NewDecoder(r.Body).Decode(&filters); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.cfg.Lock()
	s.cfg.Filters = filters
	s.cfg.Unlock()

	if err := s.cfg.Save(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, filters)
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel, err := updater.CheckLatest()
	if err != nil {
		writeJSON(w, updater.Status{Phase: "error", Error: err.Error()})
		return
	}

	writeJSON(w, updater.Status{
		Phase:   "available",
		Version: rel.TagName,
		Message: fmt.Sprintf("Latest release: %s", rel.TagName),
	})
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel, err := updater.CheckLatest()
	if err != nil {
		writeJSON(w, updater.Status{Phase: "error", Error: err.Error()})
		return
	}

	// Send the response before applying — the process is about to exit.
	writeJSON(w, updater.Status{
		Phase:   "applying",
		Version: rel.TagName,
		Message: "Downloading and restarting...",
	})

	// Flush the response so the client gets it.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Apply in a goroutine so the HTTP response completes.
	go func() {
		if err := updater.Apply(rel); err != nil {
			log.Printf("[UPDATE] failed: %v", err)
		}
	}()
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[WEB] json encode error: %v", err)
	}
}
