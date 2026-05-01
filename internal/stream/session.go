package stream

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
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

	// Warm-stream support: keep the camera streaming for idleTimeout after
	// the last RTSP client disconnects, so a quick reconnect can attach to
	// the running pipeline instead of paying the full HAP/encoder startup
	// cost again. Zero disables warm mode.
	idleTimeout time.Duration
	stopTimer   *time.Timer
}

func NewSession(cameraName string, idleTimeout time.Duration, logger *slog.Logger, onStart, onStop func() error) *Session {
	return &Session{
		state:       StateIdle,
		logger:      logger,
		cameraName:  cameraName,
		onStart:     onStart,
		onStop:      onStop,
		idleTimeout: idleTimeout,
	}
}

// ClientConnected is called when an RTSP client starts playing.
// Returns (freshStart, error): freshStart is true if this call triggered the
// camera to start (first client), false if the camera was already streaming.
func (s *Session) ClientConnected() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel any pending warm-mode stop. If Stop() returns false the timer
	// already fired; the callback is serialized on s.mu and will run after
	// this method returns, so we still need to decide based on current state.
	if s.stopTimer != nil {
		s.stopTimer.Stop()
		s.stopTimer = nil
	}

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
// Stops the stream if this was the last client. When idleTimeout > 0,
// the stop is deferred so a quick reconnect can attach to the running
// stream without paying HAP/encoder startup again.
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

	if s.idleTimeout > 0 {
		s.logger.Info("no clients, keeping stream warm",
			"camera", s.cameraName,
			"idleTimeout", s.idleTimeout)
		s.stopTimer = time.AfterFunc(s.idleTimeout, s.idleTimerFired)
		return nil
	}

	return s.stopLocked()
}

// idleTimerFired runs from a timer goroutine when the warm window expires.
func (s *Session) idleTimerFired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check: a client may have reconnected and ClientConnected raced
	// us, or Shutdown may have already torn things down.
	if s.clientCount > 0 || s.state != StateStreaming {
		s.stopTimer = nil
		return
	}

	s.logger.Info("warm window expired, stopping stream", "camera", s.cameraName)
	s.stopTimer = nil
	if err := s.stopLocked(); err != nil {
		s.logger.Warn("warm-stop error", "camera", s.cameraName, "error", err)
	}
}

// stopLocked runs onStop and transitions the state machine. Caller must
// hold s.mu; the lock is briefly released around the user callback.
func (s *Session) stopLocked() error {
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

// Shutdown cancels any pending warm-stop timer and stops the stream
// synchronously if it is still running. Safe to call when already idle.
func (s *Session) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopTimer != nil {
		s.stopTimer.Stop()
		s.stopTimer = nil
	}

	if s.state != StateStreaming {
		return nil
	}

	s.clientCount = 0
	return s.stopLocked()
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
