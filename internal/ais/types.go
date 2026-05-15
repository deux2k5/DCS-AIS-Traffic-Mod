package ais

// AISStreamMessage is the top-level envelope from aisstream.io WebSocket.
type AISStreamMessage struct {
	MessageType string   `json:"MessageType"`
	MetaData    MetaData `json:"MetaData"`
	Message     Message  `json:"Message"`
}

// MetaData carries per-message ship metadata.
type MetaData struct {
	MMSI     int    `json:"MMSI"`
	ShipName string `json:"ShipName"`
	ShipType int    `json:"ShipType"`
}

// Message wraps the actual report. Only one field will be populated per message.
type Message struct {
	PositionReport PositionReport `json:"PositionReport"`
	ShipStaticData ShipStaticData `json:"ShipStaticData"`
}

// PositionReport holds kinematic data from AIS.
type PositionReport struct {
	Latitude    float64 `json:"Latitude"`
	Longitude   float64 `json:"Longitude"`
	Cog         float64 `json:"Cog"`
	Sog         float64 `json:"Sog"`
	TrueHeading int     `json:"TrueHeading"`
}

// ShipStaticData holds vessel identity and dimensions (AIS message type 5).
type ShipStaticData struct {
	Type                 int       `json:"Type"`
	Dimension            Dimension `json:"Dimension"`
	MaximumStaticDraught float64   `json:"MaximumStaticDraught"`
	ImoNumber            int       `json:"ImoNumber"`
	Destination          string    `json:"Destination"`
}

// Dimension contains the four distance-from-GPS-antenna values.
// Length = A + B, Beam = C + D.
type Dimension struct {
	A int `json:"A"` // bow
	B int `json:"B"` // stern
	C int `json:"C"` // port
	D int `json:"D"` // starboard
}

// SubscribeMessage is sent to aisstream.io to start streaming.
type SubscribeMessage struct {
	APIKey             string         `json:"APIKey"`
	BoundingBoxes      [][][2]float64 `json:"BoundingBoxes"`
	FilterMessageTypes []string       `json:"FilterMessageTypes"`
}

// ShipUpdate is the parsed, cleaned data passed from the AIS client to the
// coordinator.
type ShipUpdate struct {
	MMSI        int
	Name        string
	ShipType    int
	Latitude    float64
	Longitude   float64
	Cog         float64 // degrees
	Sog         float64 // knots
	TrueHeading float64 // degrees
	Length      int     // metres (A+B), 0 if unknown
	Beam        int     // metres (C+D), 0 if unknown
}
