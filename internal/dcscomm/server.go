package dcscomm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
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

		// Set TCP_NODELAY for lower latency on small writes.
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
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

	// Set a write deadline so a slow Lua reader doesn't block the
	// coordinator indefinitely.
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = conn.Write(data)
	_ = conn.SetWriteDeadline(time.Time{}) // clear deadline

	if err != nil {
		// Connection broken or write timed out — close it so readLoop
		// unblocks and Start() can accept a new hook connection.
		_ = conn.Close()
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
			s.setConnected(false)
		}
		s.mu.Unlock()
	}
	return err
}

// SendBatch writes multiple commands in a single TCP write for efficiency.
// All commands are JSON-encoded and newline-delimited, then written as one
// buffer to reduce syscall overhead.
func (s *Server) SendBatch(cmds []Command) error {
	if len(cmds) == 0 {
		return nil
	}

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("no DCS hook connected")
	}

	// Pre-allocate a buffer for all commands.
	buf := make([]byte, 0, len(cmds)*128)
	for _, cmd := range cmds {
		data, err := json.Marshal(cmd)
		if err != nil {
			return err
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}

	_ = conn.SetWriteDeadline(time.Now().Add(time.Duration(len(cmds)*100+500) * time.Millisecond))
	_, err := conn.Write(buf)
	_ = conn.SetWriteDeadline(time.Time{})

	if err != nil {
		// Close the dead connection so readLoop unblocks.
		_ = conn.Close()
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
