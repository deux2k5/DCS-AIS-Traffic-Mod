package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"strings"

	"github.com/deux2k5/dcs-ais-traffic/internal/config"
	"github.com/deux2k5/dcs-ais-traffic/internal/servermgr"
	"github.com/deux2k5/dcs-ais-traffic/internal/updater"
)

//go:embed static
var staticFS embed.FS

// --------------------------------------------------------------------------
// Response types
// --------------------------------------------------------------------------

// GlobalStatusResponse is returned by GET /api/status.
type GlobalStatusResponse struct {
	AISConnected bool `json:"aisConnected"`
	ServerCount  int  `json:"serverCount"`
}

// ServerListResponse wraps the server summaries with global status to reduce
// the number of HTTP requests per poll cycle (was 4 requests, now 2).
type ServerListResponse struct {
	AISConnected bool            `json:"aisConnected"`
	Servers      []ServerSummary `json:"servers"`
}

// ServerSummary is a single entry in the server list. Contains all fields
// the dashboard needs so the frontend can poll a single endpoint per cycle
// instead of separate list + per-server status calls.
type ServerSummary struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Enabled        bool                `json:"enabled"`
	HookPort       int                 `json:"hookPort"`
	HookConnected  bool                `json:"hookConnected"`
	Theatre        string              `json:"theatre"`
	ShipCount      int                 `json:"shipCount"`
	SpawnedCount   int                 `json:"spawnedCount"`
	PendingCount   int                 `json:"pendingCount"`
	ModelsLoaded   int                 `json:"modelsLoaded"`
	HookDeployed   bool                `json:"hookDeployed"`
	SavedGamesPath string              `json:"savedGamesPath"`
	MaxShips       int                 `json:"maxShips"`
	UpdateSeconds  int                 `json:"updateSeconds"`
	StaleMinutes   int                 `json:"staleMinutes"`
	Categories     map[string]int      `json:"categories"`
	Filters        config.FilterConfig `json:"filters"`
}

// ServerStatusResponse is returned by GET /api/servers/{id}/status.
type ServerStatusResponse struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Enabled        bool           `json:"enabled"`
	Theatre        string         `json:"theatre"`
	ShipCount      int            `json:"shipCount"`
	SpawnedCount   int            `json:"spawnedCount"`
	PendingCount   int            `json:"pendingCount"`
	ModelsLoaded   int            `json:"modelsLoaded"`
	HookConnected  bool           `json:"hookConnected"`
	HookDeployed   bool           `json:"hookDeployed"`
	SavedGamesPath string         `json:"savedGamesPath"`
	HookPort       int            `json:"hookPort"`
	MaxShips       int            `json:"maxShips"`
	UpdateSeconds  int            `json:"updateSeconds"`
	StaleMinutes   int            `json:"staleMinutes"`
	Categories     map[string]int `json:"categories"`
	Filters        config.FilterConfig `json:"filters"`
}

// AddServerRequest is the body of POST /api/servers.
type AddServerRequest struct {
	Name           string `json:"name"`
	SavedGamesPath string `json:"savedGamesPath"`
}

// ToggleRequest is the body of POST /api/servers/{id}/toggle.
type ToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// --------------------------------------------------------------------------
// Server
// --------------------------------------------------------------------------

// Server is the HTTP server providing the REST API and static dashboard.
type Server struct {
	cfg *config.Config
	mgr *servermgr.Manager
}

// NewServer creates a web server.
func NewServer(cfg *config.Config, mgr *servermgr.Manager) *Server {
	return &Server{cfg: cfg, mgr: mgr}
}

// Start begins serving HTTP. This blocks.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Global API routes.
	mux.HandleFunc("GET /api/status", s.handleGlobalStatus)
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("POST /api/config", s.handleConfigPost)
	mux.HandleFunc("POST /api/browse-folder", s.handleBrowseFolder)
	mux.HandleFunc("GET /api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", s.handleUpdateApply)

	// Server CRUD.
	mux.HandleFunc("GET /api/servers", s.handleListServers)
	mux.HandleFunc("POST /api/servers", s.handleAddServer)
	mux.HandleFunc("GET /api/servers/{id}/status", s.handleServerStatus)
	mux.HandleFunc("PATCH /api/servers/{id}", s.handleUpdateServer)
	mux.HandleFunc("DELETE /api/servers/{id}", s.handleDeleteServer)

	// Per-server actions.
	mux.HandleFunc("GET /api/servers/{id}/ships", s.handleServerShips)
	mux.HandleFunc("POST /api/servers/{id}/toggle", s.handleServerToggle)
	mux.HandleFunc("POST /api/servers/{id}/filters", s.handleServerFilters)
	mux.HandleFunc("POST /api/servers/{id}/deploy", s.handleServerDeploy)

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

// --------------------------------------------------------------------------
// Global endpoints
// --------------------------------------------------------------------------

func (s *Server) handleGlobalStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, GlobalStatusResponse{
		AISConnected: s.mgr.AISClient().IsConnected(),
		ServerCount:  len(s.mgr.Instances()),
	})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	snap := s.cfg.Snapshot()
	snap.AIS.APIKey = "" // never expose over the network
	writeJSON(w, snap)
}

func (s *Server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	var incoming struct {
		AIS struct {
			APIKey string `json:"apiKey"`
		} `json:"ais"`
	}
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
	s.cfg.Unlock()

	if err := s.cfg.Save(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if keyChanged {
		s.mgr.RefreshAIS()
	}

	snap := s.cfg.Snapshot()
	snap.AIS.APIKey = ""
	writeJSON(w, snap)
}

func (s *Server) handleBrowseFolder(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		http.Error(w, "folder browser only supported on Windows", http.StatusBadRequest)
		return
	}

	// FolderBrowserDialog requires STA threading. Use -STA flag and
	// explicitly call ShowDialog with a dummy owner window handle.
	script := `Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.Application]::EnableVisualStyles()
$f = New-Object System.Windows.Forms.FolderBrowserDialog
$f.Description = "Select DCS Saved Games folder"
$f.ShowNewFolderButton = $false
$result = $f.ShowDialog()
if ($result -eq [System.Windows.Forms.DialogResult]::OK) {
  Write-Output $f.SelectedPath
}`

	cmd := exec.Command("powershell", "-STA", "-NoProfile", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[WEB] folder browse error: %v", err)
		http.Error(w, "folder dialog failed", http.StatusInternalServerError)
		return
	}

	path := strings.TrimSpace(string(out))
	writeJSON(w, map[string]string{"path": path})
}

// --------------------------------------------------------------------------
// Server CRUD
// --------------------------------------------------------------------------

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	s.cfg.RLock()
	servers := make([]config.ServerConfig, len(s.cfg.Servers))
	copy(servers, s.cfg.Servers)
	s.cfg.RUnlock()

	summaries := make([]ServerSummary, 0, len(servers))
	for _, srv := range servers {
		inst := s.mgr.Instance(srv.ID)
		summary := ServerSummary{
			ID:             srv.ID,
			Name:           srv.Name,
			Enabled:        srv.Enabled,
			HookPort:       srv.HookPort,
			SavedGamesPath: srv.SavedGamesPath,
			MaxShips:       srv.MaxShips,
			UpdateSeconds:  srv.UpdateSeconds,
			StaleMinutes:   srv.StaleMinutes,
			Filters:        srv.Filters,
		}
		if inst != nil {
			summary.HookConnected = inst.DCSServer.IsConnected()
			summary.Theatre = inst.Coordinator.Theatre()
			summary.HookDeployed = inst.HookDeployed
			stats := inst.Coordinator.Stats()
			summary.ShipCount = stats.Total
			summary.SpawnedCount = stats.Spawned
			summary.PendingCount = stats.Pending
			summary.ModelsLoaded = stats.ModelsLoaded
			summary.Categories = stats.Categories
		}
		summaries = append(summaries, summary)
	}

	writeJSON(w, ServerListResponse{
		AISConnected: s.mgr.AISClient().IsConnected(),
		Servers:      summaries,
	})
}

func (s *Server) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var req AddServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Generate a unique slug ID.
	s.cfg.RLock()
	id := s.cfg.UniqueSlug(req.Name)
	port := s.cfg.NextFreePort()
	s.cfg.RUnlock()

	srv := config.DefaultServerConfig()
	srv.ID = id
	srv.Name = req.Name
	srv.HookPort = port
	srv.SavedGamesPath = req.SavedGamesPath

	if err := s.mgr.AddServer(srv); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"id": id})
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var patch config.ServerConfig
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pathChanged := patch.SavedGamesPath != ""

	s.cfg.Lock()
	err := s.cfg.UpdateServer(id, patch)
	s.cfg.Unlock()

	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if pathChanged {
		s.mgr.RefreshHookStatus(id)
	}

	writeJSON(w, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := s.mgr.RemoveServer(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	writeJSON(w, map[string]string{"status": "removed"})
}

// --------------------------------------------------------------------------
// Per-server endpoints
// --------------------------------------------------------------------------

func (s *Server) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst := s.mgr.Instance(id)
	if inst == nil {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}

	s.cfg.RLock()
	srv := s.cfg.ServerByID(id)
	if srv == nil {
		s.cfg.RUnlock()
		http.Error(w, "server not found in config", http.StatusNotFound)
		return
	}

	resp := ServerStatusResponse{
		ID:             srv.ID,
		Name:           srv.Name,
		Enabled:        srv.Enabled,
		SavedGamesPath: srv.SavedGamesPath,
		HookPort:       srv.HookPort,
		MaxShips:       srv.MaxShips,
		UpdateSeconds:  srv.UpdateSeconds,
		StaleMinutes:   srv.StaleMinutes,
		Filters:        srv.Filters,
	}
	s.cfg.RUnlock()

	stats := inst.Coordinator.Stats()
	resp.Theatre = inst.Coordinator.Theatre()
	resp.ShipCount = stats.Total
	resp.SpawnedCount = stats.Spawned
	resp.PendingCount = stats.Pending
	resp.ModelsLoaded = stats.ModelsLoaded
	resp.HookConnected = inst.DCSServer.IsConnected()
	resp.HookDeployed = inst.HookDeployed
	resp.Categories = stats.Categories

	writeJSON(w, resp)
}

func (s *Server) handleServerShips(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst := s.mgr.Instance(id)
	if inst == nil {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}

	ships := inst.Coordinator.Ships()
	writeJSON(w, ships)
}

func (s *Server) handleServerToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst := s.mgr.Instance(id)
	if inst == nil {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}

	var req ToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	inst.Coordinator.Toggle(req.Enabled)

	// Refresh AIS since enabled/disabled affects which theatres are active.
	s.mgr.RefreshAIS()

	writeJSON(w, map[string]bool{"enabled": req.Enabled})
}

func (s *Server) handleServerFilters(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var filters config.FilterConfig
	if err := json.NewDecoder(r.Body).Decode(&filters); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.cfg.Lock()
	srv := s.cfg.ServerByID(id)
	if srv == nil {
		s.cfg.Unlock()
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	srv.Filters = filters
	s.cfg.Unlock()

	if err := s.cfg.Save(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, filters)
}

func (s *Server) handleServerDeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := s.mgr.DeployHook(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]string{
		"status":  "deployed",
		"message": "Hook deployed. Restart DCS or reload the mission for changes to take effect.",
	})
}

// --------------------------------------------------------------------------
// Update endpoints
// --------------------------------------------------------------------------

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
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
	rel, err := updater.CheckLatest()
	if err != nil {
		writeJSON(w, updater.Status{Phase: "error", Error: err.Error()})
		return
	}

	writeJSON(w, updater.Status{
		Phase:   "applying",
		Version: rel.TagName,
		Message: "Downloading and restarting...",
	})

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		if err := updater.Apply(rel); err != nil {
			log.Printf("[UPDATE] failed: %v", err)
		}
	}()
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[WEB] json encode error: %v", err)
	}
}
