package dcscomm

// Command is a JSON command sent from Go to the DCS Lua hook.
type Command struct {
	Cmd       string  `json:"cmd"`
	GroupName string  `json:"groupName,omitempty"`
	UnitType  string  `json:"unitType,omitempty"`
	Lat       float64 `json:"lat,omitempty"`
	Lon       float64 `json:"lon,omitempty"`
	Heading   float64 `json:"heading,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Name      string  `json:"name,omitempty"`
	Static    bool    `json:"static,omitempty"` // spawn as static object (anchored ships)
}

// InboundMessage is a JSON message received from the Lua hook.
type InboundMessage struct {
	Type    string   `json:"type"`
	Theatre string   `json:"theatre,omitempty"`
	Ships   int      `json:"ships,omitempty"`
	Error   string   `json:"error,omitempty"`
	Models  []string `json:"models,omitempty"` // available ship unit types
}

// NewSpawn creates a spawn command. If static is true, the hook spawns a static
// object instead of an AI group (for anchored/stationary ships).
func NewSpawn(groupName, unitType string, lat, lon, heading, speed float64, name string, static bool) Command {
	return Command{
		Cmd:       "spawn",
		GroupName: groupName,
		UnitType:  unitType,
		Lat:       lat,
		Lon:       lon,
		Heading:   heading,
		Speed:     speed,
		Name:      name,
		Static:    static,
	}
}

// NewRemove creates a remove command.
func NewRemove(groupName string) Command {
	return Command{
		Cmd:       "remove",
		GroupName: groupName,
	}
}

// NewReroute creates a reroute command (smooth waypoint update, no respawn).
// Includes unitType and name so the hook can fall back to a spawn if the group
// is missing (e.g. after mission reload).
func NewReroute(groupName, unitType string, lat, lon, heading, speed float64, name string) Command {
	return Command{
		Cmd:       "reroute",
		GroupName: groupName,
		UnitType:  unitType,
		Lat:       lat,
		Lon:       lon,
		Heading:   heading,
		Speed:     speed,
		Name:      name,
	}
}

// NewMove creates a move command (remove + spawn in DCS, used for large jumps).
func NewMove(groupName, unitType string, lat, lon, heading, speed float64, name string) Command {
	return Command{
		Cmd:       "move",
		GroupName: groupName,
		UnitType:  unitType,
		Lat:       lat,
		Lon:       lon,
		Heading:   heading,
		Speed:     speed,
		Name:      name,
	}
}

// NewClear creates a clear-all command.
func NewClear() Command {
	return Command{Cmd: "clear"}
}
