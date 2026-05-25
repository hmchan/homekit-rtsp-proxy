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

	// stopDone is created when entering StateStopping and closed when the
	// state returns to Idle (or transitions to Starting in Restart). Clients
	// that arrive during StateStopping wait on this channel and retry rather
	// than failing immediately.
	stopDone chan struct{}
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
//
// If the stream is currently stopping (StateStopping), this method blocks
// until the stop completes (up to 20 seconds) and then retries, so callers
// never see a spurious "cannot start in state stopping" error.
func (s *Session) ClientConnected() (bool, error) {
	for {
		s.mu.Lock()

		// Cancel any pending warm-mode stop. If Stop() returns false the timer
		// already fired; the callback is serialized on s.mu and will run after
		// this lock is released, so we still need to decide based on current state.
		if s.stopTimer != nil {
			s.stopTimer.Stop()
			s.stopTimer = nil
		}

		switch s.state {
		case StateStreaming, StateStarting:
			// Stream already running — attach this client and return immediately.
			s.clientCount++
			s.logger.Info("RTSP client connected",
				"camera", s.cameraName,
				"clients", s.clientCount,
				"state", s.state)
			s.mu.Unlock()
			return false, nil

		case StateStopping:
			// A previous stop is in progress (e.g. broken HAP pipe). Rather than
			// failing immediately — which would leak clientCount — wait for it to
			// finish and retry. clientCount is NOT incremented until we succeed.
			done := s.stopDone
			s.logger.Info("RTSP client waiting for in-progress stop",
				"camera", s.cameraName)
			s.mu.Unlock()
			select {
			case <-done:
				// Stop finished; loop and try again.
			case <-time.After(20 * time.Second):
				return false, fmt.Errorf("timeout waiting for stream to stop before starting")
			}
			continue

		case StateIdle:
			// First client — we will start the stream below.
			s.clientCount++
			s.logger.Info("RTSP client connected",
				"camera", s.cameraName,
				"clients", s.clientCount,
				"state", s.state)
			s.state = StateStarting
			s.mu.Unlock()

			err := s.onStart()

			s.mu.Lock()
			if err != nil {
				s.state = StateIdle
				s.clientCount--
				s.mu.Unlock()
				return false, fmt.Errorf("start stream: %w", err)
			}
			s.state = StateStreaming
			s.mu.Unlock()
			return true, nil

		default:
			s.mu.Unlock()
			return false, fmt.Errorf("cannot start stream in state %s", s.state)
		}
	}
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
// It creates stopDone before releasing the lock and closes it after
// the state returns to Idle, so ClientConnected callers waiting on
// StateStopping are unblocked as soon as the stop completes.
func (s *Session) stopLocked() error {
	s.state = StateStopping
	done := make(chan struct{})
	s.stopDone = done
	s.mu.Unlock()

	err := s.onStop()

	s.mu.Lock()
	s.state = StateIdle
	s.stopDone = nil
	close(done)
	if err != nil {
		return fmt.Errorf("stop stream: %w", err)
	}
	return nil
}

// Restart drives onStop followed by onStart while preserving the current
// client count. Used after a HAP auto-recovery so the stream picks up the
// fresh session and SRTP keys without disturbing connected RTSP clients
// (whose seq/ts continuity is maintained by the RTSP server's own state).
func (s *Session) Restart() error {
	s.mu.Lock()
	if s.state != StateStreaming {
		s.mu.Unlock()
		return nil
	}

	if s.stopTimer != nil {
		s.stopTimer.Stop()
		s.stopTimer = nil
	}

	clients := s.clientCount
	s.state = StateStopping
	done := make(chan struct{})
	s.stopDone = done
	s.mu.Unlock()

	if err := s.onStop(); err != nil {
		// onStop talking to a freshly rebooted camera will often fail
		// (session ID is gone) — that's expected, keep going.
		s.logger.Warn("onStop during restart (continuing)",
			"camera", s.cameraName, "error", err)
	}

	s.mu.Lock()
	s.stopDone = nil
	close(done)
	s.state = StateStarting
	s.mu.Unlock()

	if err := s.onStart(); err != nil {
		s.mu.Lock()
		s.state = StateIdle
		s.clientCount = 0
		s.mu.Unlock()
		return fmt.Errorf("onStart during restart: %w", err)
	}

	s.mu.Lock()
	s.state = StateStreaming
	s.clientCount = clients
	s.mu.Unlock()

	s.logger.Info("session restarted after recovery",
		"camera", s.cameraName, "clients", clients)
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
