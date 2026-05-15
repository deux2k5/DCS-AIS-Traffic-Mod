package servermgr

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/deux2k5/dcs-ais-traffic/internal/ais"
	"github.com/deux2k5/dcs-ais-traffic/internal/config"
	"github.com/deux2k5/dcs-ais-traffic/internal/coordinator"
	"github.com/deux2k5/dcs-ais-traffic/internal/dcscomm"
	"github.com/deux2k5/dcs-ais-traffic/internal/geo"
	"github.com/deux2k5/dcs-ais-traffic/internal/hookdeploy"
	"github.com/deux2k5/dcs-ais-traffic/internal/shipcache"
)

// ServerInstance holds the per-server components.
type ServerInstance struct {
	ID           string
	Coordinator  *coordinator.Coordinator
	DCSServer    *dcscomm.Server
	HookDeployed bool // cached deploy status (avoids disk I/O on every poll)
}

// Manager orchestrates multiple DCS server instances. It owns the shared AIS
// client and fans out AIS updates to each server's coordinator.
type Manager struct {
	cfg    *config.Config
	cache  *shipcache.Cache
	aisCli *ais.Client

	mu        sync.RWMutex
	instances map[string]*ServerInstance // keyed by server ID

	// Debounce timer for AIS resubscription. Theatre changes, server
	// add/remove, and toggle events all call refreshAIS(). The timer
	// collapses rapid successive events into a single resubscription.
	debounceMu    sync.Mutex
	debounceTimer *time.Timer
}

// New creates a Manager. The AIS client is created internally with a fan-out
// callback that distributes updates to all active coordinators.
func New(cfg *config.Config, cache *shipcache.Cache) *Manager {
	m := &Manager{
		cfg:       cfg,
		cache:     cache,
		instances: make(map[string]*ServerInstance),
	}

	// Create the shared AIS client with a fan-out callback.
	m.aisCli = ais.NewClient(func(u ais.ShipUpdate) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		for _, inst := range m.instances {
			inst.Coordinator.OnAISUpdate(u)
		}
	})

	return m
}

// AISClient returns the shared AIS client (for status checks).
func (m *Manager) AISClient() *ais.Client {
	return m.aisCli
}

// StartAll initializes and starts all configured servers. Failures on
// individual servers are logged but don't prevent other servers from starting.
func (m *Manager) StartAll() {
	m.cfg.RLock()
	servers := make([]config.ServerConfig, len(m.cfg.Servers))
	copy(servers, m.cfg.Servers)
	m.cfg.RUnlock()

	for i := range servers {
		if err := m.startInstance(&servers[i]); err != nil {
			log.Printf("[MGR] failed to start server %q: %v", servers[i].ID, err)
		}
	}
}

// StopAll shuts down all server instances and the AIS client.
func (m *Manager) StopAll() {
	m.mu.Lock()
	for id, inst := range m.instances {
		log.Printf("[MGR] stopping server %q", id)
		inst.Coordinator.Stop()
		inst.DCSServer.Stop()
	}
	m.instances = make(map[string]*ServerInstance)
	m.mu.Unlock()

	m.aisCli.Stop()
}

// Instance returns the server instance for the given ID, or nil.
func (m *Manager) Instance(id string) *ServerInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
}

// Instances returns a snapshot of all server instances.
func (m *Manager) Instances() []*ServerInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*ServerInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst)
	}
	return out
}

// AddServer validates, persists, and starts a new server instance.
func (m *Manager) AddServer(srv config.ServerConfig) error {
	m.cfg.Lock()
	if err := m.cfg.AddServer(srv); err != nil {
		m.cfg.Unlock()
		return err
	}
	// Get a pointer to the newly added server in the config slice.
	added := m.cfg.ServerByID(srv.ID)
	m.cfg.Unlock()

	if added == nil {
		return fmt.Errorf("server %q not found after adding", srv.ID)
	}

	if err := m.startInstance(added); err != nil {
		return fmt.Errorf("starting server: %w", err)
	}

	// Deploy hook if saved games path is set.
	if srv.SavedGamesPath != "" {
		if err := hookdeploy.Deploy(srv.SavedGamesPath, srv.HookPort); err != nil {
			log.Printf("[MGR] hook deploy warning for %q: %v", srv.ID, err)
		} else {
			log.Printf("[MGR] hook deployed to %s", hookdeploy.HookPath(srv.SavedGamesPath))
		}
	}

	return nil
}

// RemoveServer stops and removes a server instance, cleans up the hook file,
// and removes the config entry.
func (m *Manager) RemoveServer(id string) error {
	// Stop the instance first (clears ships while TCP is still up).
	m.mu.Lock()
	inst, ok := m.instances[id]
	if ok {
		inst.Coordinator.Stop()
		inst.DCSServer.Stop()
		delete(m.instances, id)
	}
	m.mu.Unlock()

	// Remove hook file if deployed.
	m.cfg.RLock()
	srv := m.cfg.ServerByID(id)
	var savedPath string
	if srv != nil {
		savedPath = srv.SavedGamesPath
	}
	m.cfg.RUnlock()

	if savedPath != "" {
		if err := hookdeploy.Remove(savedPath); err != nil {
			log.Printf("[MGR] hook remove warning for %q: %v", id, err)
		}
	}

	// Remove from config and save.
	m.cfg.Lock()
	err := m.cfg.RemoveServer(id)
	m.cfg.Unlock()

	if err != nil {
		return err
	}

	// Refresh AIS subscription since theatre list may have changed.
	m.scheduleRefreshAIS()
	return nil
}

// DeployHook deploys or redeploys the Lua hook for a server.
// Updates the cached deploy status on the instance.
func (m *Manager) DeployHook(id string) error {
	m.cfg.RLock()
	srv := m.cfg.ServerByID(id)
	if srv == nil {
		m.cfg.RUnlock()
		return fmt.Errorf("server %q not found", id)
	}
	path := srv.SavedGamesPath
	port := srv.HookPort
	m.cfg.RUnlock()

	if path == "" {
		return fmt.Errorf("server %q has no saved games path configured", id)
	}

	if err := hookdeploy.Deploy(path, port); err != nil {
		return err
	}

	// Update cached deploy status.
	m.mu.RLock()
	if inst, ok := m.instances[id]; ok {
		inst.HookDeployed = true
	}
	m.mu.RUnlock()

	return nil
}

// DeployAllHooks redeploys hooks to all configured servers that have a saved
// games path. Used on startup to keep hooks in sync with the binary version.
func (m *Manager) DeployAllHooks() {
	m.cfg.RLock()
	servers := make([]config.ServerConfig, len(m.cfg.Servers))
	copy(servers, m.cfg.Servers)
	m.cfg.RUnlock()

	for _, srv := range servers {
		if srv.SavedGamesPath == "" {
			continue
		}
		if err := hookdeploy.Deploy(srv.SavedGamesPath, srv.HookPort); err != nil {
			log.Printf("[MGR] hook deploy warning for %q: %v", srv.ID, err)
		} else {
			log.Printf("[MGR] hook deployed for %q at %s", srv.ID, hookdeploy.HookPath(srv.SavedGamesPath))
		}
	}
}

// RefreshAIS rebuilds the AIS bounding box list from all active theatres and
// restarts the AIS client. Safe to call from any goroutine.
func (m *Manager) RefreshAIS() {
	m.cfg.RLock()
	apiKey := m.cfg.AIS.APIKey
	m.cfg.RUnlock()

	if apiKey == "" {
		m.aisCli.Stop()
		return
	}

	boxes := m.activeBoxes()
	if len(boxes) == 0 {
		m.aisCli.Stop()
		log.Println("[MGR] no active theatres, AIS client stopped")
		return
	}

	m.aisCli.Restart(apiKey, boxes)
	log.Printf("[MGR] AIS resubscribed with %d bounding box(es)", len(boxes))
}

// activeBoxes collects unique bounding boxes from all enabled servers that have
// a detected theatre. Careful lock ordering: config lock first, then m.mu, never
// nested, to avoid deadlock with RemoveServer which takes m.mu then cfg.
func (m *Manager) activeBoxes() [][][2]float64 {
	// Snapshot enabled server IDs from config (cfg lock only).
	m.cfg.RLock()
	enabledSet := make(map[string]bool, len(m.cfg.Servers))
	for _, srv := range m.cfg.Servers {
		if srv.Enabled {
			enabledSet[srv.ID] = true
		}
	}
	m.cfg.RUnlock()

	// Now iterate instances under m.mu (no cfg lock held).
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	var boxes [][][2]float64

	for _, inst := range m.instances {
		if !enabledSet[inst.ID] {
			continue
		}

		theatre := inst.Coordinator.Theatre()
		if theatre == "" || seen[theatre] {
			continue
		}

		bb, ok := geo.TheatreBounds[theatre]
		if !ok {
			continue
		}

		seen[theatre] = true
		boxes = append(boxes, bb.AISBox())
	}

	return boxes
}

// scheduleRefreshAIS debounces AIS resubscription. Multiple rapid calls
// (e.g., three servers detecting theatres during startup) collapse into
// a single resubscription after 500ms of quiet.
func (m *Manager) scheduleRefreshAIS() {
	m.debounceMu.Lock()
	defer m.debounceMu.Unlock()

	if m.debounceTimer != nil {
		m.debounceTimer.Stop()
	}
	m.debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
		m.RefreshAIS()
	})
}

// startInstance creates and starts a single server's TCP listener and
// coordinator. The config pointer must point into m.cfg.Servers.
func (m *Manager) startInstance(srvCfg *config.ServerConfig) error {
	id := srvCfg.ID

	m.mu.Lock()
	if _, exists := m.instances[id]; exists {
		m.mu.Unlock()
		return fmt.Errorf("server %q already running", id)
	}
	m.mu.Unlock()

	// Create the coordinator and DCS server with a closure to forward messages.
	var coord *coordinator.Coordinator
	dcsServer := dcscomm.NewServer(srvCfg.HookPort, func(msg dcscomm.InboundMessage) {
		coord.OnHookMessage(msg)
	})
	coord = coordinator.New(id, srvCfg, m.cfg, dcsServer, m.cache, func() {
		m.scheduleRefreshAIS()
	})

	// Start TCP listener.
	go func() {
		if err := dcsServer.Start(); err != nil {
			var opErr *net.OpError
			if errors.As(err, &opErr) && opErr.Op == "accept" {
				log.Printf("[MGR:%s] TCP server stopped", id)
				return
			}
			log.Printf("[MGR:%s] TCP server error: %v", id, err)
		}
	}()

	// Start coordinator tick loop.
	go coord.Start()

	// Cache hook deploy status to avoid disk I/O on every poll.
	var hookDeployed bool
	if srvCfg.SavedGamesPath != "" {
		hookDeployed, _ = hookdeploy.IsDeployed(srvCfg.SavedGamesPath, srvCfg.HookPort)
	}

	inst := &ServerInstance{
		ID:           id,
		Coordinator:  coord,
		DCSServer:    dcsServer,
		HookDeployed: hookDeployed,
	}

	m.mu.Lock()
	m.instances[id] = inst
	m.mu.Unlock()

	log.Printf("[MGR] started server %q on port %d", id, srvCfg.HookPort)
	return nil
}
