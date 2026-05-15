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

// Coordinator manages the lifecycle of tracked ships. It connects the AIS
// feed to the DCS hook via spawn/move/remove commands.
type Coordinator struct {
	cfg    *config.Config
	dcs    *dcscomm.Server
	aisCli *ais.Client
	cache  *shipcache.Cache

	mu      sync.RWMutex
	ships   map[int]*TrackedShip // keyed by MMSI
	theatre string
	stopCh  chan struct{}
}

// New creates a Coordinator.
func New(cfg *config.Config, dcs *dcscomm.Server, cache *shipcache.Cache) *Coordinator {
	c := &Coordinator{
		cfg:    cfg,
		dcs:    dcs,
		cache:  cache,
		ships:  make(map[int]*TrackedShip),
		stopCh: make(chan struct{}),
	}
	c.aisCli = ais.NewClient(c.onAISUpdate)
	return c
}

// AISClient returns the underlying AIS client for status queries.
func (c *Coordinator) AISClient() *ais.Client {
	return c.aisCli
}

// Theatre returns the current DCS theatre name.
func (c *Coordinator) Theatre() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.theatre
}

// SetTheatre updates the theatre and restarts the AIS feed for the new bounds.
// This is called when the hook sends a "theatre" message, which happens at every
// mission load. All spawned ships are reset to pending so they get re-spawned,
// since the hook clears its tracked groups on mission load.
func (c *Coordinator) SetTheatre(name string) {
	c.mu.Lock()
	c.theatre = name
	resetCount := 0
	for _, s := range c.ships {
		if s.State == StateSpawned {
			s.State = StatePending
			s.GroupName = "" // will be re-frozen at next spawn
			resetCount++
		}
	}
	c.mu.Unlock()

	log.Printf("[COORD] theatre set to %s, reset %d ships to pending", name, resetCount)
	c.restartAIS()
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

// ShipCount returns the number of tracked ships.
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
		Total:      len(c.ships),
		Categories: make(map[string]int),
	}

	modelMu.RLock()
	if modelLoaded {
		s.ModelsLoaded = len(modelSet)
	}
	modelMu.RUnlock()

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
		SetAvailableModels(msg.Models)
		log.Printf("[DCS] %d ship models available", len(msg.Models))
	case "status":
		log.Printf("[DCS] status: %d ships", msg.Ships)
	case "error":
		log.Printf("[DCS] error: %s (group: %s)", msg.Error, msg.GroupName)
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
			log.Printf("[COORD] ship %d rejected by hook: %s", mmsi, msg.Reason)
			delete(c.ships, mmsi)
			return
		}
	}
}

// Start begins the coordinator tick loop. Call in a goroutine.
func (c *Coordinator) Start() {
	c.cfg.RLock()
	enabled := c.cfg.AIS.Enabled
	c.cfg.RUnlock()

	if enabled {
		c.restartAIS()
	}

	c.cfg.RLock()
	interval := time.Duration(c.cfg.AIS.UpdateSeconds) * time.Second
	c.cfg.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			c.aisCli.Stop()
			return
		case <-ticker.C:
			c.tick()

			// Re-read interval in case it changed.
			c.cfg.RLock()
			newInterval := time.Duration(c.cfg.AIS.UpdateSeconds) * time.Second
			c.cfg.RUnlock()
			if newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

// Stop signals the coordinator to shut down.
func (c *Coordinator) Stop() {
	close(c.stopCh)
	c.cache.Stop()
}

// Toggle enables or disables AIS tracking.
func (c *Coordinator) Toggle(enabled bool) {
	c.cfg.Lock()
	c.cfg.AIS.Enabled = enabled
	c.cfg.Unlock()
	_ = c.cfg.Save()

	if enabled {
		c.restartAIS()
	} else {
		c.disable()
	}
}

func (c *Coordinator) disable() {
	c.aisCli.Stop()

	// Send clear to DCS.
	if err := c.dcs.Send(dcscomm.NewClear()); err != nil {
		log.Printf("[COORD] clear error: %v", err)
	}

	// Mark all ships removed.
	c.mu.Lock()
	c.ships = make(map[int]*TrackedShip)
	c.mu.Unlock()

	log.Println("[COORD] disabled, all ships cleared")
}

// RestartAIS triggers a reconnect of the AIS WebSocket client (e.g. after an
// API key change).
func (c *Coordinator) RestartAIS() {
	c.restartAIS()
}

func (c *Coordinator) restartAIS() {
	c.cfg.RLock()
	apiKey := c.cfg.AIS.APIKey
	enabled := c.cfg.AIS.Enabled
	c.cfg.RUnlock()

	if !enabled || apiKey == "" {
		c.aisCli.Stop()
		return
	}

	c.mu.RLock()
	theatre := c.theatre
	c.mu.RUnlock()

	bb, ok := geo.TheatreBounds[theatre]
	if !ok {
		log.Printf("[COORD] unknown theatre %q, AIS stopped", theatre)
		c.aisCli.Stop()
		return
	}

	boxes := [][][2]float64{bb.AISBox()}
	c.aisCli.Restart(apiKey, boxes)
}

func (c *Coordinator) onAISUpdate(u ais.ShipUpdate) {
	// Validate position.
	if u.Latitude == 0 && u.Longitude == 0 {
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

	// Check filter.
	c.cfg.RLock()
	allowed := isFilterAllowed(c.cfg.Filters, category)
	c.cfg.RUnlock()

	if !allowed {
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
			DCSModel: DCSUnitType(shipType, length),
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

	// Update ship type if we got a real type (from ShipStaticData).
	if u.ShipType > 0 && ship.ShipType == 0 {
		ship.ShipType = u.ShipType
		ship.Category = ShipCategory(u.ShipType)
	}

	// Update dimensions if we got new data.
	if u.Length > 0 && u.Length != ship.Length {
		ship.Length = u.Length
		ship.Beam = u.Beam
	}

	// Re-pick model while still pending.
	if ship.State == StatePending && (u.Length > 0 || u.ShipType > 0) {
		ship.DCSModel = DCSUnitType(ship.ShipType, ship.Length)
	}

	// Persist to disk cache for next restart.
	c.cache.Update(u.MMSI, ship.ShipType, ship.Length, ship.Beam, ship.Name)
}

// tickSnapshot holds per-ship data captured under lock for use outside the lock.
type tickSnapshot struct {
	mmsi    int
	ship    TrackedShip // value copy
	shipPtr *TrackedShip
}

func (c *Coordinator) tick() {
	c.cfg.RLock()
	enabled := c.cfg.AIS.Enabled
	maxShips := c.cfg.AIS.MaxShips
	staleMinutes := c.cfg.AIS.StaleMinutes
	c.cfg.RUnlock()

	if !enabled {
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

	// Collect pending ships sorted by SOG descending.
	type pendingEntry struct {
		mmsi int
		ship *TrackedShip
	}
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

	const staticThreshold = 0.5

	// Build spawn commands. State changes are deferred until after a
	// successful TCP send so a failed batch doesn't leave the coordinator
	// out of sync with DCS.
	type spawnCommit struct {
		ship     *TrackedShip
		name     string
		isStatic bool
	}
	var spawnCmds []dcscomm.Command
	var spawnCommits []spawnCommit
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

	// Build reroute/conversion commands for spawned ships.
	type updateCommit struct {
		ship       *TrackedShip
		lat, lon   float64
		hdg        float64
		isStatic   bool      // new value after conversion
		converted  bool      // static→group conversion
		rerouted   bool
		rerouteAt  time.Time
	}
	var updateCmds []dcscomm.Command
	var updateCommits []updateCommit
	for _, s := range c.ships {
		if s.State != StateSpawned || s.GroupName == "" {
			continue
		}

		headingRad := s.Heading * math.Pi / 180.0
		speedMS := s.Sog * 0.514444

		// Static→group conversion if the ship starts moving.
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

		// Per-ship reroute cooldown to avoid command churn from heading jitter.
		if now.Sub(s.LastReroute) < minRerouteInterval {
			continue
		}

		// Equirectangular distance approximation — much cheaper than haversine
		// and accurate enough for a 200m threshold check.
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
			log.Printf("[COORD] batch send error (%d cmds): %v", len(allCmds), err)
			// Don't commit state changes — the commands didn't reach DCS.
			// They'll be retried on the next tick.
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
	// Trim trailing spaces (AIS names are space-padded).
	end := len(name)
	for end > 0 && name[end-1] == ' ' {
		end--
	}
	if end == 0 {
		return "UNKNOWN"
	}
	return name[:end]
}
