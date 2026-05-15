package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	toml "github.com/pelletier/go-toml/v2"
)

// ConfigData holds the serializable configuration fields.
type ConfigData struct {
	AIS     AISConfig      `toml:"ais"     json:"ais"`
	Web     WebConfig      `toml:"web"     json:"web"`
	Servers []ServerConfig `toml:"servers" json:"servers"`
}

// ServerConfig holds per-DCS-server settings.
type ServerConfig struct {
	ID             string       `toml:"id"               json:"id"`
	Name           string       `toml:"name"             json:"name"`
	HookPort       int          `toml:"hook-port"        json:"hookPort"`
	SavedGamesPath string       `toml:"saved-games-path" json:"savedGamesPath"`
	Enabled        bool         `toml:"enabled"          json:"enabled"`
	MaxShips       int          `toml:"max-ships"        json:"maxShips"`
	UpdateSeconds  int          `toml:"update-seconds"   json:"updateSeconds"`
	StaleMinutes   int          `toml:"stale-minutes"    json:"staleMinutes"`
	Filters        FilterConfig `toml:"filters"          json:"filters"`
}

// Config holds all application configuration.
type Config struct {
	mu   sync.RWMutex `toml:"-"`
	path string       `toml:"-"`

	ConfigData
}

// AISConfig holds the global AIS settings (shared across all servers).
type AISConfig struct {
	APIKey string `toml:"api-key" json:"apiKey"`
}

type WebConfig struct {
	Port int `toml:"port" json:"port"`
}

type FilterConfig struct {
	Fishing   bool `toml:"fishing"   json:"fishing"`
	Cargo     bool `toml:"cargo"     json:"cargo"`
	Tanker    bool `toml:"tanker"    json:"tanker"`
	Passenger bool `toml:"passenger" json:"passenger"`
	Tug       bool `toml:"tug"       json:"tug"`
	Pleasure  bool `toml:"pleasure"  json:"pleasure"`
	Other     bool `toml:"other"     json:"other"`
}

// defaults returns a Config with default values (no servers configured).
func defaults() *Config {
	return &Config{
		ConfigData: ConfigData{
			AIS: AISConfig{
				APIKey: "",
			},
			Web: WebConfig{
				Port: 8380,
			},
			Servers: []ServerConfig{},
		},
	}
}

// DefaultFilters returns a FilterConfig with all categories enabled.
func DefaultFilters() FilterConfig {
	return FilterConfig{
		Fishing:   true,
		Cargo:     true,
		Tanker:    true,
		Passenger: true,
		Tug:       true,
		Pleasure:  true,
		Other:     true,
	}
}

// DefaultServerConfig returns a ServerConfig with sensible defaults.
// ID, Name, HookPort, and SavedGamesPath must be set by the caller.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Enabled:       true,
		MaxShips:      100,
		UpdateSeconds: 30,
		StaleMinutes:  10,
		Filters:       DefaultFilters(),
	}
}

// --------------------------------------------------------------------------
// V1 migration types (old flat config format)
// --------------------------------------------------------------------------

// v1Config is the old single-server config format for migration detection.
type v1Config struct {
	AIS     v1AISConfig `toml:"ais"`
	DCS     v1DCSConfig `toml:"dcs"`
	Web     WebConfig   `toml:"web"`
	Filters FilterConfig `toml:"filters"`
}

type v1AISConfig struct {
	APIKey        string `toml:"api-key"`
	Enabled       bool   `toml:"enabled"`
	MaxShips      int    `toml:"max-ships"`
	UpdateSeconds int    `toml:"update-seconds"`
	StaleMinutes  int    `toml:"stale-minutes"`
}

type v1DCSConfig struct {
	HookPort int `toml:"hook-port"`
}

// isV1Format detects the old config format by checking for a [dcs] section.
func isV1Format(data []byte) bool {
	// Simple heuristic: old format has [dcs] section, new format has [[servers]].
	hasDCS := strings.Contains(string(data), "[dcs]") ||
		strings.Contains(string(data), "hook-port")
	hasServers := strings.Contains(string(data), "[[servers]]")
	return hasDCS && !hasServers
}

// migrateV1 converts old flat config to the new multi-server format.
func migrateV1(data []byte) (*Config, error) {
	var old v1Config
	if err := toml.Unmarshal(data, &old); err != nil {
		return nil, fmt.Errorf("parsing v1 config: %w", err)
	}

	hookPort := old.DCS.HookPort
	if hookPort == 0 {
		hookPort = 18420
	}

	maxShips := old.AIS.MaxShips
	if maxShips == 0 {
		maxShips = 100
	}

	updateSecs := old.AIS.UpdateSeconds
	if updateSecs == 0 {
		updateSecs = 30
	}

	staleMins := old.AIS.StaleMinutes
	if staleMins == 0 {
		staleMins = 10
	}

	webPort := old.Web.Port
	if webPort == 0 {
		webPort = 8380
	}

	cfg := &Config{
		ConfigData: ConfigData{
			AIS: AISConfig{
				APIKey: old.AIS.APIKey,
			},
			Web: WebConfig{
				Port: webPort,
			},
			Servers: []ServerConfig{
				{
					ID:             "server-1",
					Name:           "DCS Server",
					HookPort:       hookPort,
					SavedGamesPath: "", // user must set via UI
					Enabled:        false,
					MaxShips:       maxShips,
					UpdateSeconds:  updateSecs,
					StaleMinutes:   staleMins,
					Filters:        old.Filters,
				},
			},
		},
	}

	// Only default filters when the old config had no [filters] section at all.
	// If the section existed (even with all categories false), the user
	// intentionally set those values and we must preserve them.
	hasFilterSection := strings.Contains(string(data), "[filters]")
	if !hasFilterSection {
		cfg.Servers[0].Filters = DefaultFilters()
	}

	return cfg, nil
}

// --------------------------------------------------------------------------
// Load / Save
// --------------------------------------------------------------------------

// Load reads a TOML config from the given path. If the file does not exist it
// creates one with default values and returns that. Old v1 format configs are
// auto-migrated and saved in the new format.
func Load(path string) (*Config, error) {
	cfg := defaults()
	cfg.path = path

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := cfg.save(); saveErr != nil {
				return nil, saveErr
			}
			return cfg, nil
		}
		return nil, err
	}

	// Detect and migrate old config format.
	if isV1Format(data) {
		migrated, err := migrateV1(data)
		if err != nil {
			return nil, fmt.Errorf("config migration failed: %w", err)
		}
		migrated.path = path
		if saveErr := migrated.save(); saveErr != nil {
			return nil, fmt.Errorf("saving migrated config: %w", saveErr)
		}
		return migrated, nil
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the current config to disk.
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.save()
}

func (c *Config) save() error {
	data, err := toml.Marshal(c.ConfigData)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0644)
}

// --------------------------------------------------------------------------
// Locking
// --------------------------------------------------------------------------

// Lock provides write access to modify config fields. Call Unlock when done.
func (c *Config) Lock() {
	c.mu.Lock()
}

// Unlock releases the write lock.
func (c *Config) Unlock() {
	c.mu.Unlock()
}

// RLock provides read access.
func (c *Config) RLock() {
	c.mu.RLock()
}

// RUnlock releases the read lock.
func (c *Config) RUnlock() {
	c.mu.RUnlock()
}

// Snapshot returns a copy of the config safe for serialization (no mutex).
func (c *Config) Snapshot() ConfigData {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Deep copy the servers slice so the caller can't mutate ours.
	servers := make([]ServerConfig, len(c.Servers))
	copy(servers, c.Servers)

	return ConfigData{
		AIS:     c.AIS,
		Web:     c.Web,
		Servers: servers,
	}
}

// --------------------------------------------------------------------------
// Server CRUD
// --------------------------------------------------------------------------

// ServerByID returns a pointer to the server config with the given ID.
// The caller must hold at least a read lock.
func (c *Config) ServerByID(id string) *ServerConfig {
	for i := range c.Servers {
		if c.Servers[i].ID == id {
			return &c.Servers[i]
		}
	}
	return nil
}

// ServerIDs returns the IDs of all configured servers.
func (c *Config) ServerIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, len(c.Servers))
	for i, s := range c.Servers {
		ids[i] = s.ID
	}
	return ids
}

// NextFreePort returns the next available hook port by scanning existing servers.
func (c *Config) NextFreePort() int {
	maxPort := 18419
	for _, s := range c.Servers {
		if s.HookPort > maxPort {
			maxPort = s.HookPort
		}
	}
	return maxPort + 1
}

// AddServer validates and adds a server config. Caller must hold the write lock.
// Saves to disk after adding.
func (c *Config) AddServer(srv ServerConfig) error {
	// Validate ID.
	if srv.ID == "" {
		return fmt.Errorf("server ID is required")
	}
	if c.ServerByID(srv.ID) != nil {
		return fmt.Errorf("server ID %q already exists", srv.ID)
	}

	// Validate port.
	if srv.HookPort < 1024 || srv.HookPort > 65535 {
		return fmt.Errorf("hook port %d out of valid range (1024-65535)", srv.HookPort)
	}
	if srv.HookPort == c.Web.Port {
		return fmt.Errorf("hook port %d conflicts with web server port", srv.HookPort)
	}
	for _, s := range c.Servers {
		if s.HookPort == srv.HookPort {
			return fmt.Errorf("hook port %d already used by server %q", srv.HookPort, s.Name)
		}
	}

	// Validate no duplicate saved-games path.
	if srv.SavedGamesPath != "" {
		for _, s := range c.Servers {
			if strings.EqualFold(s.SavedGamesPath, srv.SavedGamesPath) {
				return fmt.Errorf("saved games path already used by server %q", s.Name)
			}
		}
	}

	// Apply defaults for zero values.
	if srv.MaxShips == 0 {
		srv.MaxShips = 100
	}
	if srv.UpdateSeconds == 0 {
		srv.UpdateSeconds = 30
	}
	if srv.StaleMinutes == 0 {
		srv.StaleMinutes = 10
	}

	c.Servers = append(c.Servers, srv)
	return c.save()
}

// RemoveServer removes the server with the given ID. Caller must hold the write lock.
// Saves to disk after removing.
func (c *Config) RemoveServer(id string) error {
	for i, s := range c.Servers {
		if s.ID == id {
			c.Servers = append(c.Servers[:i], c.Servers[i+1:]...)
			return c.save()
		}
	}
	return fmt.Errorf("server %q not found", id)
}

// UpdateServer merges non-zero fields from patch into the existing server config.
// Caller must hold the write lock. Saves to disk after updating.
func (c *Config) UpdateServer(id string, patch ServerConfig) error {
	srv := c.ServerByID(id)
	if srv == nil {
		return fmt.Errorf("server %q not found", id)
	}

	if patch.Name != "" {
		srv.Name = patch.Name
	}
	if patch.SavedGamesPath != "" {
		srv.SavedGamesPath = patch.SavedGamesPath
	}
	if patch.MaxShips > 0 {
		srv.MaxShips = patch.MaxShips
	}
	if patch.UpdateSeconds > 0 {
		srv.UpdateSeconds = patch.UpdateSeconds
	}
	if patch.StaleMinutes > 0 {
		srv.StaleMinutes = patch.StaleMinutes
	}

	return c.save()
}

// --------------------------------------------------------------------------
// Slug helper
// --------------------------------------------------------------------------

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a human-readable name into a URL-safe ID.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "server"
	}
	return s
}

// UniqueSlug generates a slug that doesn't collide with existing server IDs.
func (c *Config) UniqueSlug(name string) string {
	base := Slugify(name)
	slug := base
	n := 2
	for c.ServerByID(slug) != nil {
		slug = fmt.Sprintf("%s-%d", base, n)
		n++
	}
	return slug
}
