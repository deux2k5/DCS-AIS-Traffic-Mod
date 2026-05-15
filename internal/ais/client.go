package ais

import (
	"encoding/json"
	"log"
	"math"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const wsURL = "wss://stream.aisstream.io/v0/stream"

// dimCache stores ship dimensions and type keyed by MMSI, populated from ShipStaticData.
type dimEntry struct {
	Length   int
	Beam     int
	ShipType int
}

// Client manages a WebSocket connection to aisstream.io.
type Client struct {
	mu        sync.Mutex
	apiKey    string
	boxes     [][][2]float64
	onUpdate  func(ShipUpdate)
	conn      *websocket.Conn
	stopCh    chan struct{}
	running   bool
	connected bool
	connMu    sync.RWMutex

	dims   map[int]dimEntry
	dimsMu sync.RWMutex
}

// NewClient creates an AIS streaming client.
func NewClient(onUpdate func(ShipUpdate)) *Client {
	return &Client{
		onUpdate: onUpdate,
		dims:     make(map[int]dimEntry),
	}
}

// IsConnected returns the current WebSocket connection status.
func (c *Client) IsConnected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.connected
}

func (c *Client) setConnected(v bool) {
	c.connMu.Lock()
	c.connected = v
	c.connMu.Unlock()
}

// Start begins the WebSocket connection in a new goroutine. It is safe to call
// multiple times; duplicate calls are ignored.
func (c *Client) Start(apiKey string, boxes [][][2]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return
	}

	c.apiKey = apiKey
	c.boxes = boxes
	c.stopCh = make(chan struct{})
	c.running = true

	go c.loop()
}

// Stop closes the WebSocket and terminates the read loop.
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return
	}

	close(c.stopCh)
	c.running = false
	c.setConnected(false)

	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// Restart stops and starts with new parameters.
func (c *Client) Restart(apiKey string, boxes [][][2]float64) {
	c.Stop()
	c.Start(apiKey, boxes)
}

func (c *Client) loop() {
	backoff := time.Second

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		err := c.connect()
		if err != nil {
			log.Printf("[AIS] connection error: %v (retry in %v)", err, backoff)
			c.setConnected(false)
			select {
			case <-time.After(backoff):
			case <-c.stopCh:
				return
			}
			backoff = time.Duration(math.Min(float64(backoff*2), float64(60*time.Second)))
			continue
		}

		// Connected successfully, reset backoff.
		backoff = time.Second
		c.setConnected(true)
		log.Println("[AIS] connected to aisstream.io")

		c.readLoop()
		c.setConnected(false)
		log.Println("[AIS] disconnected from aisstream.io")
	}
}

func (c *Client) connect() error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}

	sub := SubscribeMessage{
		APIKey:             c.apiKey,
		BoundingBoxes:      c.boxes,
		FilterMessageTypes: []string{"PositionReport", "ShipStaticData"},
	}

	if err := conn.WriteJSON(sub); err != nil {
		_ = conn.Close()
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	return nil
}

func (c *Client) readLoop() {
	// Capture conn locally to avoid racing with Stop() which nils c.conn.
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg AISStreamMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[AIS] parse error: %v", err)
			continue
		}

		switch msg.MessageType {
		case "PositionReport":
			c.handlePosition(msg)
		case "ShipStaticData":
			c.handleStaticData(msg)
		}
	}
}

func (c *Client) handlePosition(msg AISStreamMessage) {
	pr := msg.Message.PositionReport

	// Skip invalid positions.
	if pr.Latitude == 0 && pr.Longitude == 0 {
		return
	}

	heading := float64(pr.TrueHeading)
	if pr.TrueHeading == 511 { // 511 = not available, fall back to COG
		heading = pr.Cog
	}

	// Look up cached dimensions for this MMSI.
	c.dimsMu.RLock()
	dim, hasDim := c.dims[msg.MetaData.MMSI]
	c.dimsMu.RUnlock()

	update := ShipUpdate{
		MMSI:        msg.MetaData.MMSI,
		Name:        msg.MetaData.ShipName,
		ShipType:    msg.MetaData.ShipType,
		Latitude:    pr.Latitude,
		Longitude:   pr.Longitude,
		Cog:         pr.Cog,
		Sog:         pr.Sog,
		TrueHeading: heading,
	}

	if hasDim {
		update.Length = dim.Length
		update.Beam = dim.Beam
		if update.ShipType == 0 && dim.ShipType > 0 {
			update.ShipType = dim.ShipType
		}
	}

	c.onUpdate(update)
}

func (c *Client) handleStaticData(msg AISStreamMessage) {
	sd := msg.Message.ShipStaticData
	length := sd.Dimension.A + sd.Dimension.B
	beam := sd.Dimension.C + sd.Dimension.D

	if length <= 0 && beam <= 0 && sd.Type <= 0 {
		return
	}

	c.dimsMu.Lock()
	// Merge with existing entry so we don't lose data from earlier messages.
	existing := c.dims[msg.MetaData.MMSI]
	if length > 0 {
		existing.Length = length
	}
	if beam > 0 {
		existing.Beam = beam
	}
	if sd.Type > 0 {
		existing.ShipType = sd.Type
	}
	c.dims[msg.MetaData.MMSI] = existing
	c.dimsMu.Unlock()
}
