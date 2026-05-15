// Package shipcache provides a persistent on-disk cache of ship metadata
// (type, dimensions) keyed by MMSI. It survives process restarts so that
// ships seen before are immediately classified without waiting for AIS
// ShipStaticData messages.
package shipcache

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// Meta holds cached metadata for a single vessel.
type Meta struct {
	Name     string `json:"name,omitempty"`
	ShipType int    `json:"type,omitempty"`
	Length   int    `json:"length,omitempty"`
	Beam     int    `json:"beam,omitempty"`
}

// Cache is a thread-safe, auto-persisting ship metadata cache.
type Cache struct {
	mu    sync.RWMutex
	data  map[int]Meta // keyed by MMSI
	path  string
	dirty bool
	stopCh chan struct{}
}

// New loads (or creates) a cache at the given file path.
func New(path string) *Cache {
	c := &Cache{
		data:   make(map[int]Meta),
		path:   path,
		stopCh: make(chan struct{}),
	}
	c.load()
	go c.saveLoop()
	return c
}

// Lookup returns cached metadata for a given MMSI, if available.
func (c *Cache) Lookup(mmsi int) (Meta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.data[mmsi]
	return m, ok
}

// Update merges new metadata for a vessel. Only non-zero fields are written.
func (c *Cache) Update(mmsi int, shipType, length, beam int, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	m := c.data[mmsi]
	changed := false

	if shipType > 0 && m.ShipType != shipType {
		m.ShipType = shipType
		changed = true
	}
	if length > 0 && m.Length != length {
		m.Length = length
		changed = true
	}
	if beam > 0 && m.Beam != beam {
		m.Beam = beam
		changed = true
	}
	if name != "" && name != "UNKNOWN" && m.Name != name {
		m.Name = name
		changed = true
	}

	if changed {
		c.data[mmsi] = m
		c.dirty = true
	}
}

// Size returns the number of cached vessels.
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// Stop flushes to disk and stops the save loop.
func (c *Cache) Stop() {
	close(c.stopCh)
	c.save()
}

func (c *Cache) load() {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[CACHE] load error: %v", err)
		}
		return
	}

	var m map[int]Meta
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("[CACHE] parse error: %v", err)
		return
	}

	c.data = m
	log.Printf("[CACHE] loaded %d vessels from %s", len(m), c.path)
}

func (c *Cache) save() {
	c.mu.RLock()
	dirty := c.dirty
	if !dirty {
		c.mu.RUnlock()
		return
	}
	// Copy data under read lock.
	data, err := json.MarshalIndent(c.data, "", "  ")
	c.mu.RUnlock()

	if err != nil {
		log.Printf("[CACHE] marshal error: %v", err)
		return
	}

	if err := os.WriteFile(c.path, data, 0644); err != nil {
		log.Printf("[CACHE] write error: %v", err)
		return
	}

	c.mu.Lock()
	c.dirty = false
	c.mu.Unlock()
}

func (c *Cache) saveLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.save()
		}
	}
}
