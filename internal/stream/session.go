package stream

import (
	"fmt"
	"log/slog"
	"sync"
)

// SessionState represents the lifecycle state of a camera stream.
type SessionState int

const (
	StateIdle     SessionState = iota
	StateStarting
	StateStreaming
	StateStopping
)

func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateStarting:
		return "starting"
	case StateStreaming:
		return "streaming"
	case StateStopping:
		return "stopping"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// Session tracks the state of a single camera stream and its RTSP clients.
type Session struct {
	mu          sync.Mutex
	state       SessionState
	clientCount int
	logger      *slog.Logger
	cameraName  string

	// SessionID from HAP SetupEndpoints exchange.
	sessionID [16]byte

	// Callbacks for lifecycle transitions.
	onStart func() error
	onStop  func() error
}

func NewSession(cameraName string, logger *slog.Logger, onStart, onStop func() error) *Session {
	return &Session{
		state:      StateIdle,
		logger:     logger,
		cameraName: cameraName,
		onStart:    onStart,
		onStop:     onStop,
	}
}

// ClientConnected is called when an RTSP client starts playing.
// Returns (freshStart, error): freshStart is true if this call triggered the
// camera to start (first client), false if the camera was already streaming.
func (s *Session) ClientConnected() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clientCount++
	s.logger.Info("RTSP client connected",
		"camera", s.cameraName,
		"clients", s.clientCount,
		"state", s.state)

	if s.state == StateStreaming || s.state == StateStarting {
		return false, nil
	}

	if s.state != StateIdle {
		return false, fmt.Errorf("cannot start stream in state %s", s.state)
	}

	s.state = StateStarting
	s.mu.Unlock()

	err := s.onStart()

	s.mu.Lock()
	if err != nil {
		s.state = StateIdle
		s.clientCount--
		return false, fmt.Errorf("start stream: %w", err)
	}

	s.state = StateStreaming
	return true, nil
}

// ClientDisconnected is called when an RTSP client disconnects.
// Stops the stream if this was the last client.
func (s *Session) ClientDisconnected() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.clientCount <= 0 {
		return nil
	}

	s.clientCount--
	s.logger.Info("RTSP client disconnected",
		"camera", s.cameraName,
		"clients", s.clientCount,
		"state", s.state)

	if s.clientCount > 0 {
		return nil
	}

	if s.state != StateStreaming {
		return nil
	}

	s.state = StateStopping
	s.mu.Unlock()

	err := s.onStop()

	s.mu.Lock()
	s.state = StateIdle
	if err != nil {
		return fmt.Errorf("stop stream: %w", err)
	}

	return nil
}

// State returns the current session state.
func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SetSessionID stores the HAP session ID for later use in stop commands.
func (s *Session) SetSessionID(id [16]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionID = id
}

// GetSessionID returns the current session ID.
func (s *Session) GetSessionID() [16]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}
