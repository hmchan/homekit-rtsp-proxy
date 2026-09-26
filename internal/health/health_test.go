package health

import (
	"testing"
	"time"
)

const wedge204 = "start HAP stream: read SetupEndpoints response: HTTP 204: "

func testReporter(wedgeAfter time.Duration) (*Reporter, *camState) {
	r := &Reporter{cfg: Config{WedgeAfter: wedgeAfter}}
	c := &camState{slug: "cam", name: "Cam", since: time.Unix(0, 0)}
	return r, c
}

// A sustained failure streak flips to a problem only once WedgeAfter elapses.
func TestFlagsAfterSustainedFailure(t *testing.T) {
	r, c := testReporter(90 * time.Second)
	base := time.Unix(1000, 0)

	if r.applyEvent(c, event{errText: wedge204}, base) {
		t.Fatal("state should not change on the first failure")
	}
	if c.problem {
		t.Fatal("flagged a problem far too early")
	}
	if r.applyEvent(c, event{errText: wedge204}, base.Add(30*time.Second)); c.problem {
		t.Fatal("flagged before WedgeAfter elapsed")
	}
	if !r.applyEvent(c, event{errText: wedge204}, base.Add(90*time.Second)) || !c.problem {
		t.Fatal("expected a problem to be flagged once WedgeAfter elapsed")
	}
}

// A single successful start clears the problem and resets the streak.
func TestRecoveryClearsProblem(t *testing.T) {
	r, c := testReporter(60 * time.Second)
	base := time.Unix(2000, 0)

	r.applyEvent(c, event{errText: wedge204}, base)
	r.applyEvent(c, event{errText: wedge204}, base.Add(60*time.Second))
	if !c.problem {
		t.Fatal("expected a problem before recovery")
	}
	if !r.applyEvent(c, event{ok: true}, base.Add(70*time.Second)) || c.problem {
		t.Fatal("a successful start should clear the problem")
	}
	if c.failCount != 0 || !c.firstFail.IsZero() {
		t.Fatalf("recovery should reset the streak, got failCount=%d firstFail=%v", c.failCount, c.firstFail)
	}
}

// Attempts resuming after a long pause start a fresh streak rather than
// instantly looking wedged based on a stale first-failure timestamp.
func TestStaleGapRestartsStreak(t *testing.T) {
	r, c := testReporter(60 * time.Second)
	base := time.Unix(3000, 0)

	r.applyEvent(c, event{errText: wedge204}, base)
	changed := r.applyEvent(c, event{errText: wedge204}, base.Add(staleGap+time.Minute))
	if changed || c.problem {
		t.Fatal("a failure after a long pause must restart the streak, not flag")
	}
	if c.failCount != 1 {
		t.Fatalf("expected a fresh streak of 1, got %d", c.failCount)
	}
}

// The known camera-side wedge is distinguished from a generic outage.
func TestWedgeSuspectedClassification(t *testing.T) {
	if !isSetupEndpointsWedge(wedge204) {
		t.Fatal("SetupEndpoints HTTP 204 should be classified as the wedge")
	}
	if isSetupEndpointsWedge("start HAP stream: dial tcp 192.168.101.196:80: connect: no route to host") {
		t.Fatal("a generic connectivity outage should not be classified as the wedge")
	}
}

func dropReporter(threshold float64, window, clear time.Duration) (*Reporter, *camState) {
	r := &Reporter{cfg: Config{DropThreshold: threshold, DropWindow: window, DropClearWindow: clear}}
	c := &camState{slug: "cam", name: "Cam", dropSince: time.Unix(0, 0)}
	return r, c
}

// Sustained high packet loss flags frame drops only after DropWindow.
func TestFrameDropsFlagAfterSustainedLoss(t *testing.T) {
	r, c := dropReporter(0.05, 20*time.Second, 30*time.Second)
	base := time.Unix(1000, 0)

	// 10% loss (5 of 50) each second.
	if r.applyStats(c, lossFraction(45, 5), 5, base) {
		t.Fatal("should not flag on the first high-loss sample")
	}
	if r.applyStats(c, lossFraction(45, 5), 5, base.Add(19*time.Second)); c.degraded {
		t.Fatal("flagged before DropWindow elapsed")
	}
	if !r.applyStats(c, lossFraction(45, 5), 5, base.Add(20*time.Second)) || !c.degraded {
		t.Fatal("expected frame drops to be flagged after DropWindow of high loss")
	}
}

// A brief loss spike that clears within the dead band never flags.
func TestFrameDropsIgnoreBriefSpike(t *testing.T) {
	r, c := dropReporter(0.05, 20*time.Second, 30*time.Second)
	base := time.Unix(2000, 0)

	r.applyStats(c, lossFraction(40, 10), 10, base) // 20% loss, streak starts
	// Loss returns to zero well before DropWindow.
	r.applyStats(c, 0, 0, base.Add(2*time.Second))
	// Even much later, a single high sample must not instantly flag.
	if r.applyStats(c, lossFraction(40, 10), 10, base.Add(60*time.Second)) || c.degraded {
		t.Fatal("a brief spike followed by recovery must not flag frame drops")
	}
}

// Once flagged, frame drops clear only after sustained low loss (hysteresis).
func TestFrameDropsClearAfterRecovery(t *testing.T) {
	r, c := dropReporter(0.05, 10*time.Second, 30*time.Second)
	base := time.Unix(3000, 0)

	r.applyStats(c, lossFraction(40, 10), 10, base)
	if !r.applyStats(c, lossFraction(40, 10), 10, base.Add(10*time.Second)) || !c.degraded {
		t.Fatal("expected degraded before recovery")
	}
	// Clean samples begin; must stay clean for DropClearWindow before clearing.
	r.applyStats(c, 0, 0, base.Add(11*time.Second))
	if r.applyStats(c, 0, 0, base.Add(20*time.Second)); !c.degraded {
		t.Fatal("cleared before DropClearWindow elapsed")
	}
	if !r.applyStats(c, 0, 0, base.Add(41*time.Second)) || c.degraded {
		t.Fatal("expected frame drops to clear after sustained low loss")
	}
}

// A fresh stream session clears stale degraded state.
func TestResetDropClearsDegraded(t *testing.T) {
	r, c := dropReporter(0.05, 10*time.Second, 30*time.Second)
	base := time.Unix(4000, 0)
	r.applyStats(c, lossFraction(40, 10), 10, base)
	r.applyStats(c, lossFraction(40, 10), 10, base.Add(10*time.Second))
	if !c.degraded {
		t.Fatal("expected degraded")
	}
	if !r.resetDrop(c, base.Add(11*time.Second)) || c.degraded {
		t.Fatal("a fresh stream session should clear degraded state")
	}
}

func TestLossFraction(t *testing.T) {
	if got := lossFraction(0, 0); got != 0 {
		t.Fatalf("no traffic should be 0 loss, got %v", got)
	}
	if got := lossFraction(90, 10); got != 0.1 {
		t.Fatalf("expected 0.1, got %v", got)
	}
}
