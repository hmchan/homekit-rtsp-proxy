package onvif

import (
	"sync"
	"time"
)

// MotionEvent represents a motion detection event.
type MotionEvent struct {
	Time     time.Time
	IsMotion bool
}

// PullPointSubscription manages a single ONVIF PullPoint subscription.
type PullPointSubscription struct {
	ID              string
	Created         time.Time
	TerminationTime time.Time

	mu     sync.Mutex
	events []MotionEvent
	notify chan struct{}
}

func NewPullPointSubscription(id string, timeout time.Duration) *PullPointSubscription {
	return &PullPointSubscription{
		ID:              id,
		Created:         time.Now(),
		TerminationTime: time.Now().Add(timeout),
		notify:          make(chan struct{}, 1),
	}
}

// PushEvent adds a motion event and wakes any waiting PullMessages call.
func (pp *PullPointSubscription) PushEvent(evt MotionEvent) {
	pp.mu.Lock()
	pp.events = append(pp.events, evt)
	pp.mu.Unlock()

	// Non-blocking signal to wake the long-poll.
	select {
	case pp.notify <- struct{}{}:
	default:
	}
}

// PullMessages waits for events up to the given timeout.
// Returns any accumulated events.
func (pp *PullPointSubscription) PullMessages(timeout time.Duration) []MotionEvent {
	// Check for existing events first.
	pp.mu.Lock()
	if len(pp.events) > 0 {
		events := pp.events
		pp.events = nil
		pp.mu.Unlock()
		return events
	}
	pp.mu.Unlock()

	// Long-poll: wait for events or timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-pp.notify:
	case <-timer.C:
	}

	pp.mu.Lock()
	events := pp.events
	pp.events = nil
	pp.mu.Unlock()

	return events
}

// Renew extends the subscription termination time.
func (pp *PullPointSubscription) Renew(timeout time.Duration) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.TerminationTime = time.Now().Add(timeout)
}

// IsExpired returns true if the subscription has passed its termination time.
func (pp *PullPointSubscription) IsExpired() bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return time.Now().After(pp.TerminationTime)
}

// PullPointManager manages multiple PullPoint subscriptions and fans out events.
type PullPointManager struct {
	mu            sync.RWMutex
	subscriptions map[string]*PullPointSubscription
}

func NewPullPointManager() *PullPointManager {
	return &PullPointManager{
		subscriptions: make(map[string]*PullPointSubscription),
	}
}

// Create creates a new subscription and returns it.
func (m *PullPointManager) Create(id string, timeout time.Duration) *PullPointSubscription {
	sub := NewPullPointSubscription(id, timeout)
	m.mu.Lock()
	m.subscriptions[id] = sub
	m.mu.Unlock()
	return sub
}

// Get returns a subscription by ID.
func (m *PullPointManager) Get(id string) *PullPointSubscription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.subscriptions[id]
}

// Remove deletes a subscription.
func (m *PullPointManager) Remove(id string) {
	m.mu.Lock()
	delete(m.subscriptions, id)
	m.mu.Unlock()
}

// FanOut sends a motion event to all active subscriptions.
func (m *PullPointManager) FanOut(evt MotionEvent) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, sub := range m.subscriptions {
		if !sub.IsExpired() {
			sub.PushEvent(evt)
		}
	}
}

// CleanExpired removes expired subscriptions.
func (m *PullPointManager) CleanExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, sub := range m.subscriptions {
		if sub.IsExpired() {
			delete(m.subscriptions, id)
		}
	}
}
