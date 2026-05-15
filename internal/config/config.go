package config

import (
	"os"
	"sync"

	toml "github.com/pelletier/go-toml/v2"
)

// ConfigData holds the serializable configuration fields.
type ConfigData struct {
	AIS     AISConfig    `toml:"ais"     json:"ais"`
	DCS     DCSConfig    `toml:"dcs"     json:"dcs"`
	Web     WebConfig    `toml:"web"     json:"web"`
	Filters FilterConfig `toml:"filters" json:"filters"`
}

// Config holds all application configuration.
type Config struct {
	mu   sync.RWMutex `toml:"-"`
	path string       `toml:"-"`

	ConfigData
}

type AISConfig struct {
	APIKey        string `toml:"api-key"        json:"apiKey"`
	Enabled       bool   `toml:"enabled"        json:"enabled"`
	MaxShips      int    `toml:"max-ships"      json:"maxShips"`
	UpdateSeconds int    `toml:"update-seconds" json:"updateSeconds"`
	StaleMinutes  int    `toml:"stale-minutes"  json:"staleMinutes"`
}

type DCSConfig struct {
	HookPort int `toml:"hook-port" json:"hookPort"`
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

// defaults returns a Config with default values.
func defaults() *Config {
	return &Config{
		ConfigData: ConfigData{
			AIS: AISConfig{
				APIKey:        "",
				Enabled:       false,
				MaxShips:      100,
				UpdateSeconds: 30,
				StaleMinutes:  10,
			},
			DCS: DCSConfig{
				HookPort: 18420,
			},
			Web: WebConfig{
				Port: 8380,
			},
			Filters: FilterConfig{
				Fishing:   true,
				Cargo:     true,
				Tanker:    true,
				Passenger: true,
				Tug:       true,
				Pleasure:  true,
				Other:     true,
			},
		},
	}
}

// Load reads a TOML config from the given path. If the file does not exist it
// creates one with default values and returns that.
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
	return ConfigData{
		AIS:     c.AIS,
		DCS:     c.DCS,
		Web:     c.Web,
		Filters: c.Filters,
	}
}
