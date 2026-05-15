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
	State      ShipState `json:"state"`
	LastSeen   time.Time `json:"lastSeen"`
	SpawnedLat float64   `json:"-"`
	SpawnedLon float64   `json:"-"`
	SpawnedHdg float64   `json:"-"`
	GroupName  string    `json:"-"` // frozen at spawn time so reroute/remove work
	IsStatic   bool      `json:"-"` // spawned as static object (no AI)
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
		log.Printf("[DCS] error: %s", msg.Error)
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

func (c *Coordinator) tick() {
	c.cfg.RLock()
	enabled := c.cfg.AIS.Enabled
	maxShips := c.cfg.AIS.MaxShips
	staleMinutes := c.cfg.AIS.StaleMinutes
	c.cfg.RUnlock()

	if !enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	staleThreshold := now.Add(-time.Duration(staleMinutes) * time.Minute)

	// Count currently spawned.
	spawned := 0
	for _, s := range c.ships {
		if s.State == StateSpawned {
			spawned++
		}
	}

	// Remove stale ships.
	for mmsi, s := range c.ships {
		if s.LastSeen.Before(staleThreshold) {
			if s.State == StateSpawned && s.GroupName != "" {
				if err := c.dcs.Send(dcscomm.NewRemove(s.GroupName)); err != nil {
					log.Printf("[COORD] remove stale %d error: %v", mmsi, err)
				}
				spawned--
			}
			delete(c.ships, mmsi)
		}
	}

	// Collect pending ships sorted by SOG descending so moving vessels are
	// spawned first when we're near the max-ships limit.
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
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].ship.Sog > pending[j].ship.Sog
	})

	const staticThreshold = 0.5 // knots — below this, ship is considered anchored

	// Spawn pending ships, prioritising those that are moving.
	for _, pe := range pending {
		if spawned >= maxShips {
			break
		}
		s := pe.ship
		// Freeze group name at spawn time so reroute/remove always use the
		// same name even if the ship's AIS name updates later.
		s.GroupName = fmt.Sprintf("%s - %d", s.Name, pe.mmsi)
		headingRad := s.Heading * math.Pi / 180.0
		speedMS := s.Sog * 0.514444
		isStatic := s.Sog < staticThreshold

		cmd := dcscomm.NewSpawn(s.GroupName, s.DCSModel, s.Lat, s.Lon, headingRad, speedMS, s.Name, isStatic)
		if err := c.dcs.Send(cmd); err != nil {
			log.Printf("[COORD] spawn %d error: %v", pe.mmsi, err)
			continue
		}
		s.State = StateSpawned
		s.IsStatic = isStatic
		s.SpawnedLat = s.Lat
		s.SpawnedLon = s.Lon
		s.SpawnedHdg = s.Heading
		spawned++
	}

	// Update spawned ships — reroute moving ones, convert static→group if they start moving.
	for mmsi, s := range c.ships {
		if s.State != StateSpawned || s.GroupName == "" {
			continue
		}

		headingRad := s.Heading * math.Pi / 180.0
		speedMS := s.Sog * 0.514444

		// If a static ship starts moving, convert to a dynamic group.
		if s.IsStatic && s.Sog >= 1.0 {
			// Remove the static object, then spawn as a group.
			if err := c.dcs.Send(dcscomm.NewRemove(s.GroupName)); err != nil {
				log.Printf("[COORD] remove static %d error: %v", mmsi, err)
				continue
			}
			cmd := dcscomm.NewSpawn(s.GroupName, s.DCSModel, s.Lat, s.Lon, headingRad, speedMS, s.Name, false)
			if err := c.dcs.Send(cmd); err != nil {
				log.Printf("[COORD] static->group %d error: %v", mmsi, err)
				continue
			}
			s.IsStatic = false
			s.SpawnedLat = s.Lat
			s.SpawnedLon = s.Lon
			s.SpawnedHdg = s.Heading
			continue
		}

		// Static ships don't need rerouting — they just sit there.
		if s.IsStatic {
			continue
		}

		dist := geo.HaversineDistance(s.SpawnedLat, s.SpawnedLon, s.Lat, s.Lon)
		hdgDiff := math.Abs(s.Heading - s.SpawnedHdg)
		if hdgDiff > 180 {
			hdgDiff = 360 - hdgDiff
		}

		if dist > 200 || hdgDiff > 5 {
			cmd := dcscomm.NewReroute(s.GroupName, s.DCSModel, s.Lat, s.Lon, headingRad, speedMS, s.Name)
			if err := c.dcs.Send(cmd); err != nil {
				log.Printf("[COORD] reroute %d error: %v", mmsi, err)
				continue
			}
			s.SpawnedLat = s.Lat
			s.SpawnedLon = s.Lon
			s.SpawnedHdg = s.Heading
		}
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
