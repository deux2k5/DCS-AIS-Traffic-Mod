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

// Server is a TCP client that connects to the DCS Lua hook's listener.
// Despite the name (kept for backward compatibility with the rest of the
// codebase), it now dials OUT to the hook rather than accepting inbound
// connections.
type Server struct {
	port      int
	onMessage OnMessage

	mu   sync.Mutex
	conn net.Conn

	connMu    sync.RWMutex
	connected bool

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewServer creates a new DCS communicator that will connect to the hook
// listening on the given port.
func NewServer(port int, onMessage OnMessage) *Server {
	return &Server{
		port:      port,
		onMessage: onMessage,
		stopCh:    make(chan struct{}),
	}
}

// IsConnected reports whether the connection to the Lua hook is active.
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

// Start begins the connect loop. It repeatedly dials the hook's TCP port and
// reads messages. This blocks, so call it in a goroutine. It returns when
// Stop() is called.
func (s *Server) Start() error {
	log.Printf("[DCS] connecting to hook on port %d", s.port)

	for {
		select {
		case <-s.stopCh:
			return nil
		default:
		}

		conn, err := s.dial()
		if err != nil {
			// Wait before retry, but respect stop signal.
			select {
			case <-s.stopCh:
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}

		// Check stop AFTER dial — Stop() may have fired while we were
		// blocking in DialTimeout. Don't publish a stale connection.
		select {
		case <-s.stopCh:
			_ = conn.Close()
			return nil
		default:
		}

		// Connected.
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.conn = conn
		s.mu.Unlock()

		s.setConnected(true)
		log.Printf("[DCS] connected to hook on port %d", s.port)

		// Read until disconnection.
		s.readLoop(conn)

		// Disconnected — clear conn.
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
			s.setConnected(false)
		}
		s.mu.Unlock()
		log.Printf("[DCS] hook disconnected (port %d)", s.port)

		// Brief pause before reconnect.
		select {
		case <-s.stopCh:
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// dial attempts a TCP connection to the hook with a short timeout.
func (s *Server) dial() (net.Conn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}

	// Set TCP_NODELAY for lower latency on small writes.
	// Enable TCP keepalive so a crashed DCS (no FIN sent) gets detected
	// within ~30s instead of hanging the readLoop forever.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(10 * time.Second)
	}

	return conn, nil
}

// Send writes a command to the connected Lua hook. Returns an error if not
// connected.
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

// Stop closes the connection and stops the reconnect loop.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})

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
