package coordinator

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/deux2k5/dcs-ais-traffic/internal/ais"
	"github.com/deux2k5/dcs-ais-traffic/internal/config"
	"github.com/deux2k5/dcs-ais-traffic/internal/dcscomm"
	"github.com/deux2k5/dcs-ais-traffic/internal/geo"
	"github.com/deux2k5/dcs-ais-traffic/internal/shipcache"
)

// ShipState tracks the lifecycle of a tracked ship.
type ShipState int

const (
	StatePending  ShipState = iota // Seen but not spawned.
	StateSpawned                   // Spawned in DCS.
	StateRemoving                  // Removal sent, awaiting cleanup.
)

// Minimum interval between reroutes for the same ship, to avoid command churn
// from AIS heading jitter.
const minRerouteInterval = 15 * time.Second

// TrackedShip holds all data for one tracked vessel.
type TrackedShip struct {
	MMSI        int       `json:"mmsi"`
	Name        string    `json:"name"`
	ShipType    int       `json:"shipType"`
	Category    string    `json:"category"`
	DCSModel    string    `json:"dcsModel"`
	Lat         float64   `json:"lat"`
	Lon         float64   `json:"lon"`
	Heading     float64   `json:"heading"` // degrees
	Sog         float64   `json:"sog"`     // knots
	Length      int       `json:"length"`  // metres (A+B from AIS)
	Beam        int       `json:"beam"`    // metres (C+D from AIS)
	State       ShipState `json:"state"`
	LastSeen    time.Time `json:"lastSeen"`
	SpawnedLat  float64   `json:"-"`
	SpawnedLon  float64   `json:"-"`
	SpawnedHdg  float64   `json:"-"`
	GroupName   string    `json:"-"` // frozen at spawn time so reroute/remove work
	IsStatic    bool      `json:"-"` // spawned as static object (no AI)
	LastReroute time.Time `json:"-"` // cooldown to prevent reroute churn
}

func (s ShipState) String() string {
	switch s {
	case StatePending:
		return "Pending"
	case StateSpawned:
		return "Spawned"
	case StateRemoving:
		return "Removing"
	default:
		return "Unknown"
	}
}

// MarshalJSON converts ShipState to its string representation for JSON.
func (s ShipState) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// OnTheatreChange is called when a coordinator detects a theatre change.
// The caller should use this to trigger an AIS resubscription with updated
// bounding boxes.
type OnTheatreChange func()

// Coordinator manages the lifecycle of tracked ships for a single DCS server
// instance. It receives AIS updates from the outside and sends spawn/move/remove
// commands to its assigned DCS hook via TCP.
type Coordinator struct {
	id        string
	serverCfg *config.ServerConfig
	globalCfg *config.Config
	dcs       *dcscomm.Server
	cache     *shipcache.Cache
	models    modelRegistry

	onTheatreChange OnTheatreChange

	mu       sync.RWMutex
	ships    map[int]*TrackedShip // keyed by MMSI
	theatre  string
	stopCh   chan struct{}
	stopOnce sync.Once
}

// New creates a Coordinator for a single DCS server instance.
func New(id string, serverCfg *config.ServerConfig, globalCfg *config.Config,
	dcs *dcscomm.Server, cache *shipcache.Cache, onTheatreChange OnTheatreChange) *Coordinator {
	return &Coordinator{
		id:              id,
		serverCfg:       serverCfg,
		globalCfg:       globalCfg,
		dcs:             dcs,
		cache:           cache,
		onTheatreChange: onTheatreChange,
		ships:           make(map[int]*TrackedShip),
		stopCh:          make(chan struct{}),
	}
}

// ID returns the server identifier for this coordinator.
func (c *Coordinator) ID() string {
	return c.id
}

// Theatre returns the current DCS theatre name.
func (c *Coordinator) Theatre() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.theatre
}

// SetTheatre updates the theatre. All spawned ships are reset to pending so
// they get re-spawned, since the hook clears its tracked groups on mission load.
// Notifies the theatre change callback so AIS bounding boxes can be refreshed.
func (c *Coordinator) SetTheatre(name string) {
	c.mu.Lock()
	c.theatre = name
	resetCount := 0
	for _, s := range c.ships {
		if s.State == StateSpawned {
			s.State = StatePending
			s.GroupName = ""
			resetCount++
		}
	}
	c.mu.Unlock()

	log.Printf("[COORD:%s] theatre set to %s, reset %d ships to pending", c.id, name, resetCount)

	if c.onTheatreChange != nil {
		c.onTheatreChange()
	}
}

// Ships returns a snapshot of all currently tracked ships.
func (c *Coordinator) Ships() []TrackedShip {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]TrackedShip, 0, len(c.ships))
	for _, s := range c.ships {
		out = append(out, *s)
	}
	return out
}

// ShipCount returns the number of tracked ships. Cheaper than Stats() when
// only the count is needed (e.g., server list endpoint).
func (c *Coordinator) ShipCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.ships)
}

// CoordStats holds aggregate statistics for the status API.
type CoordStats struct {
	Total        int
	Spawned      int
	Pending      int
	ModelsLoaded int
	Categories   map[string]int
}

// Stats returns aggregate statistics about tracked ships.
func (c *Coordinator) Stats() CoordStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := CoordStats{
		Total:        len(c.ships),
		ModelsLoaded: c.models.count(),
		Categories:   make(map[string]int),
	}

	for _, ship := range c.ships {
		switch ship.State {
		case StateSpawned:
			s.Spawned++
		case StatePending:
			s.Pending++
		}
		cat := ship.Category
		if cat == "" {
			cat = "other"
		}
		s.Categories[cat]++
	}
	return s
}

// OnHookMessage handles inbound messages from the DCS Lua hook.
func (c *Coordinator) OnHookMessage(msg dcscomm.InboundMessage) {
	switch msg.Type {
	case "theatre":
		c.SetTheatre(msg.Theatre)
	case "models":
		c.models.setAvailableModels(msg.Models)
		log.Printf("[COORD:%s] %d ship models available", c.id, len(msg.Models))
	case "status":
		log.Printf("[COORD:%s] hook status: %d ships", c.id, msg.Ships)
	case "error":
		log.Printf("[COORD:%s] hook error: %s (group: %s)", c.id, msg.Error, msg.GroupName)
	case "reject":
		c.handleReject(msg)
	}
}

// handleReject processes rejection feedback from the Lua hook (e.g. ship on land).
func (c *Coordinator) handleReject(msg dcscomm.InboundMessage) {
	if msg.GroupName == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for mmsi, s := range c.ships {
		if s.GroupName == msg.GroupName {
			log.Printf("[COORD:%s] ship %d rejected by hook: %s", c.id, mmsi, msg.Reason)
			delete(c.ships, mmsi)
			return
		}
	}
}

// OnAISUpdate processes a single AIS position/static-data update.
// Called by the server manager's fan-out callback for each AIS message.
func (c *Coordinator) OnAISUpdate(u ais.ShipUpdate) {
	// Validate position.
	if u.Latitude == 0 && u.Longitude == 0 {
		return
	}

	// Only process if tracking is enabled for this server.
	c.globalCfg.RLock()
	srv := c.serverCfg
	trackingEnabled := srv.TrackingEnabled
	filters := srv.Filters
	c.globalCfg.RUnlock()

	if !trackingEnabled {
		return
	}

	c.mu.RLock()
	theatre := c.theatre
	c.mu.RUnlock()

	bb, ok := geo.TheatreBounds[theatre]
	if !ok {
		return
	}
	if !bb.Contains(u.Latitude, u.Longitude) {
		return
	}

	category := ShipCategory(u.ShipType)

	if !isFilterAllowed(filters, category) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ship, exists := c.ships[u.MMSI]
	if !exists {
		shipType := u.ShipType
		length := u.Length
		beam := u.Beam
		name := cleanName(u.Name)

		// Pre-populate from persistent cache.
		if cached, ok := c.cache.Lookup(u.MMSI); ok {
			if shipType == 0 && cached.ShipType > 0 {
				shipType = cached.ShipType
				category = ShipCategory(shipType)
			}
			if length == 0 && cached.Length > 0 {
				length = cached.Length
			}
			if beam == 0 && cached.Beam > 0 {
				beam = cached.Beam
			}
			if (name == "" || name == "UNKNOWN") && cached.Name != "" {
				name = cached.Name
			}
		}

		ship = &TrackedShip{
			MMSI:     u.MMSI,
			Name:     name,
			ShipType: shipType,
			Category: category,
			Length:   length,
			Beam:     beam,
			DCSModel: c.models.dcsUnitType(shipType, length),
			State:    StatePending,
		}
		c.ships[u.MMSI] = ship
	}

	ship.Lat = u.Latitude
	ship.Lon = u.Longitude
	ship.Heading = u.TrueHeading
	ship.Sog = u.Sog
	ship.LastSeen = time.Now()

	if u.Name != "" {
		ship.Name = cleanName(u.Name)
	}

	if u.ShipType > 0 && ship.ShipType == 0 {
		ship.ShipType = u.ShipType
		ship.Category = ShipCategory(u.ShipType)
	}

	if u.Length > 0 && u.Length != ship.Length {
		ship.Length = u.Length
		ship.Beam = u.Beam
	}

	if ship.State == StatePending && (u.Length > 0 || u.ShipType > 0) {
		ship.DCSModel = c.models.dcsUnitType(ship.ShipType, ship.Length)
	}

	c.cache.Update(u.MMSI, ship.ShipType, ship.Length, ship.Beam, ship.Name)
}

// Start begins the coordinator tick loop. Call in a goroutine.
func (c *Coordinator) Start() {
	c.globalCfg.RLock()
	interval := time.Duration(c.serverCfg.UpdateSeconds) * time.Second
	c.globalCfg.RUnlock()

	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.tick()

			// Re-read interval in case it changed.
			c.globalCfg.RLock()
			newInterval := time.Duration(c.serverCfg.UpdateSeconds) * time.Second
			c.globalCfg.RUnlock()
			if newInterval > 0 && newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

// Stop signals the coordinator to shut down and clears all ships from DCS.
// Safe to call multiple times — subsequent calls are no-ops.
func (c *Coordinator) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.disableAll()
	})
}

// ToggleTracking enables or disables AIS data tracking for this server.
// Disabling tracking also disables spawning (can't spawn without data).
func (c *Coordinator) ToggleTracking(enabled bool) {
	c.globalCfg.Lock()
	// Update coordinator's local copy (used by tick/OnAISUpdate).
	c.serverCfg.TrackingEnabled = enabled
	if !enabled {
		c.serverCfg.SpawnEnabled = false
	}
	// Update canonical config entry (used by web API reads and disk persistence).
	if srv := c.globalCfg.ServerByID(c.id); srv != nil {
		srv.TrackingEnabled = enabled
		if !enabled {
			srv.SpawnEnabled = false
		}
	}
	c.globalCfg.Unlock()
	_ = c.globalCfg.Save()

	if !enabled {
		c.disableAll()
	}
}

// ToggleSpawning enables or disables spawning tracked ships into DCS.
// Enabling spawning also enables tracking (can't spawn without data).
// When disabled, ships remain tracked but are removed from DCS and reset
// to pending. When re-enabled, pending ships spawn on the next tick.
func (c *Coordinator) ToggleSpawning(enabled bool) {
	c.globalCfg.Lock()
	c.serverCfg.SpawnEnabled = enabled
	if enabled {
		c.serverCfg.TrackingEnabled = true
	}
	if srv := c.globalCfg.ServerByID(c.id); srv != nil {
		srv.SpawnEnabled = enabled
		if enabled {
			srv.TrackingEnabled = true
		}
	}
	c.globalCfg.Unlock()
	_ = c.globalCfg.Save()

	if !enabled {
		c.clearSpawned()
	}
}

// disableAll clears all ships from DCS and the tracking map.
func (c *Coordinator) disableAll() {
	if err := c.dcs.Send(dcscomm.NewClear()); err != nil {
		log.Printf("[COORD:%s] clear error: %v", c.id, err)
	}

	c.mu.Lock()
	c.ships = make(map[int]*TrackedShip)
	c.mu.Unlock()

	log.Printf("[COORD:%s] tracking disabled, all ships cleared", c.id)
}

// clearSpawned removes spawned ships from DCS but keeps them in the
// tracking table as pending so they can be re-spawned later.
func (c *Coordinator) clearSpawned() {
	if err := c.dcs.Send(dcscomm.NewClear()); err != nil {
		log.Printf("[COORD:%s] clear error: %v", c.id, err)
	}

	c.mu.Lock()
	for _, s := range c.ships {
		if s.State == StateSpawned {
			s.State = StatePending
			s.GroupName = ""
		}
	}
	c.mu.Unlock()

	log.Printf("[COORD:%s] spawning disabled, ships reset to pending", c.id)
}

func (c *Coordinator) tick() {
	c.globalCfg.RLock()
	trackingEnabled := c.serverCfg.TrackingEnabled
	spawnEnabled := c.serverCfg.SpawnEnabled
	maxShips := c.serverCfg.MaxShips
	staleMinutes := c.serverCfg.StaleMinutes
	c.globalCfg.RUnlock()

	if !trackingEnabled {
		return
	}

	now := time.Now()
	staleThreshold := now.Add(-time.Duration(staleMinutes) * time.Minute)

	// ---------------------------------------------------------------
	// Phase 1: snapshot state under lock, build command lists
	// ---------------------------------------------------------------
	c.mu.Lock()

	spawned := 0
	for _, s := range c.ships {
		if s.State == StateSpawned {
			spawned++
		}
	}

	// Collect stale ships to remove.
	var staleCmds []dcscomm.Command
	var staleMMSIs []int
	for mmsi, s := range c.ships {
		if s.LastSeen.Before(staleThreshold) {
			if s.State == StateSpawned && s.GroupName != "" {
				staleCmds = append(staleCmds, dcscomm.NewRemove(s.GroupName))
				spawned--
			}
			staleMMSIs = append(staleMMSIs, mmsi)
		}
	}
	for _, mmsi := range staleMMSIs {
		delete(c.ships, mmsi)
	}

	// Collect pending ships to spawn (only if spawning is enabled).
	type pendingEntry struct {
		mmsi int
		ship *TrackedShip
	}
	const staticThreshold = 0.5

	type spawnCommit struct {
		ship     *TrackedShip
		name     string
		isStatic bool
	}
	var spawnCmds []dcscomm.Command
	var spawnCommits []spawnCommit

	if spawnEnabled {
		var pending []pendingEntry
		for mmsi, s := range c.ships {
			if s.State == StatePending {
				pending = append(pending, pendingEntry{mmsi, s})
			}
		}
		if len(pending) > 1 {
			sort.Slice(pending, func(i, j int) bool {
				return pending[i].ship.Sog > pending[j].ship.Sog
			})
		}

		for _, pe := range pending {
			if spawned >= maxShips {
				break
			}
			s := pe.ship
			groupName := fmt.Sprintf("%s - %d", s.Name, pe.mmsi)
			headingRad := s.Heading * math.Pi / 180.0
			speedMS := s.Sog * 0.514444
			isStatic := s.Sog < staticThreshold

			spawnCmds = append(spawnCmds, dcscomm.NewSpawn(
				groupName, s.DCSModel, s.Lat, s.Lon, headingRad, speedMS, s.Name, isStatic))
			spawnCommits = append(spawnCommits, spawnCommit{ship: s, name: groupName, isStatic: isStatic})
			spawned++
		}
	}

	// Update/reroute spawned ships (only if spawning is enabled).
	type updateCommit struct {
		ship      *TrackedShip
		lat, lon  float64
		hdg       float64
		isStatic  bool
		converted bool
		rerouted  bool
		rerouteAt time.Time
	}
	var updateCmds []dcscomm.Command
	var updateCommits []updateCommit

	if spawnEnabled {
		for _, s := range c.ships {
			if s.State != StateSpawned || s.GroupName == "" {
				continue
			}

			headingRad := s.Heading * math.Pi / 180.0
			speedMS := s.Sog * 0.514444

			if s.IsStatic && s.Sog >= 1.0 {
				updateCmds = append(updateCmds, dcscomm.NewRemove(s.GroupName))
				updateCmds = append(updateCmds, dcscomm.NewSpawn(
					s.GroupName, s.DCSModel, s.Lat, s.Lon, headingRad, speedMS, s.Name, false))
				updateCommits = append(updateCommits, updateCommit{
					ship: s, lat: s.Lat, lon: s.Lon, hdg: s.Heading,
					isStatic: false, converted: true,
				})
				continue
			}

			if s.IsStatic {
				continue
			}

			if now.Sub(s.LastReroute) < minRerouteInterval {
				continue
			}

			dist := geo.EquirectangularDistance(s.SpawnedLat, s.SpawnedLon, s.Lat, s.Lon)
			hdgDiff := math.Abs(s.Heading - s.SpawnedHdg)
			if hdgDiff > 180 {
				hdgDiff = 360 - hdgDiff
			}

			if dist > 200 || hdgDiff > 5 {
				updateCmds = append(updateCmds, dcscomm.NewReroute(
					s.GroupName, s.DCSModel, s.Lat, s.Lon, headingRad, speedMS, s.Name))
				updateCommits = append(updateCommits, updateCommit{
					ship: s, lat: s.Lat, lon: s.Lon, hdg: s.Heading,
					rerouted: true, rerouteAt: now,
				})
			}
		}
	}

	c.mu.Unlock()

	// ---------------------------------------------------------------
	// Phase 2: send commands outside the lock via batched writes
	// ---------------------------------------------------------------
	allCmds := make([]dcscomm.Command, 0, len(staleCmds)+len(spawnCmds)+len(updateCmds))
	allCmds = append(allCmds, staleCmds...)
	allCmds = append(allCmds, spawnCmds...)
	allCmds = append(allCmds, updateCmds...)

	if len(allCmds) > 0 {
		if err := c.dcs.SendBatch(allCmds); err != nil {
			log.Printf("[COORD:%s] batch send error (%d cmds): %v", c.id, len(allCmds), err)
			return
		}
	}

	// ---------------------------------------------------------------
	// Phase 3: commit state changes under lock after successful send
	// ---------------------------------------------------------------
	if len(spawnCommits) > 0 || len(updateCommits) > 0 {
		c.mu.Lock()
		for _, sc := range spawnCommits {
			sc.ship.GroupName = sc.name
			sc.ship.State = StateSpawned
			sc.ship.IsStatic = sc.isStatic
			sc.ship.SpawnedLat = sc.ship.Lat
			sc.ship.SpawnedLon = sc.ship.Lon
			sc.ship.SpawnedHdg = sc.ship.Heading
		}
		for _, uc := range updateCommits {
			if uc.converted {
				uc.ship.IsStatic = uc.isStatic
			}
			uc.ship.SpawnedLat = uc.lat
			uc.ship.SpawnedLon = uc.lon
			uc.ship.SpawnedHdg = uc.hdg
			if uc.rerouted {
				uc.ship.LastReroute = uc.rerouteAt
			}
		}
		c.mu.Unlock()
	}
}

func isFilterAllowed(f config.FilterConfig, category string) bool {
	switch category {
	case "fishing":
		return f.Fishing
	case "cargo":
		return f.Cargo
	case "tanker":
		return f.Tanker
	case "passenger":
		return f.Passenger
	case "tug":
		return f.Tug
	case "pleasure":
		return f.Pleasure
	case "other":
		return f.Other
	default:
		return f.Other
	}
}

func cleanName(name string) string {
	end := len(name)
	for end > 0 && name[end-1] == ' ' {
		end--
	}
	if end == 0 {
		return "UNKNOWN"
	}
	return name[:end]
}
