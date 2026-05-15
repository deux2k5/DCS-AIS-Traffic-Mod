package dcscomm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

// OnMessage is a callback for messages received from the Lua hook.
type OnMessage func(InboundMessage)

// Server is a TCP server that communicates with a single DCS Lua hook client.
type Server struct {
	port      int
	onMessage OnMessage
	mu        sync.Mutex
	conn      net.Conn
	listener  net.Listener
	connMu    sync.RWMutex
	connected bool
}

// NewServer creates a new TCP server on the given port.
func NewServer(port int, onMessage OnMessage) *Server {
	return &Server{
		port:      port,
		onMessage: onMessage,
	}
}

// IsConnected reports whether the Lua hook is currently connected.
func (s *Server) IsConnected() bool {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	return s.connected
}

func (s *Server) setConnected(v bool) {
	s.connMu.Lock()
	s.connected = v
	s.connMu.Unlock()
}

// Start begins listening for TCP connections. This blocks, so call it in a
// goroutine.
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = ln
	log.Printf("[DCS] TCP server listening on %s", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		// Only allow one client at a time.
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.conn = conn
		s.mu.Unlock()

		s.setConnected(true)
		log.Printf("[DCS] hook connected from %s", conn.RemoteAddr())

		s.readLoop(conn)

		// Clear conn only if it's still the one we were reading from.
		// A newer connection may have already replaced it.
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
			s.setConnected(false)
		}
		s.mu.Unlock()
		log.Printf("[DCS] hook disconnected")
	}
}

// Send writes a command to the connected Lua hook. Returns an error if no
// client is connected.
func (s *Server) Send(cmd Command) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("no DCS hook connected")
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	_, err = conn.Write(data)
	if err != nil {
		// Connection broken; only clear if it's still the current one.
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
			s.setConnected(false)
		}
		s.mu.Unlock()
	}
	return err
}

// Stop closes the listener and any active connection.
func (s *Server) Stop() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()
	s.setConnected(false)
}

func (s *Server) readLoop(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var msg InboundMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("[DCS] parse error: %v", err)
			continue
		}
		s.onMessage(msg)
	}
}
