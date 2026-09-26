// Package health reports camera stream health to Home Assistant over MQTT.
//
// It exists for one concrete failure mode: the camera's HAP streaming
// subsystem can wedge (SetupEndpoints returns HTTP 204 forever) while the
// device otherwise stays reachable, so nothing recovers until the camera is
// power-cycled by hand. There is no remote reboot path for these cameras, so
// the proxy cannot fix it automatically — instead it raises a Home Assistant
// binary_sensor (device_class "problem") so an HA automation can notify a
// human to power-cycle the camera.
//
// It reports two conditions to Home Assistant, each as its own binary_sensor
// (device_class "problem"):
//   - "Stream problem": the camera cannot start a stream (the wedge above, or a
//     sustained outage).
//   - "Frame drops": the stream runs but sheds packets/frames frequently enough
//     to degrade the picture.
//
// The reporter is strictly best-effort and fully decoupled from the streaming
// path: the stream code calls ReportStreamStarted / ReportStreamFailed /
// ReportVideoStats, which only enqueue an event and never block. If the MQTT
// broker is down the proxy keeps proxying video; the reporter reconnects in the
// background and republishes retained state on reconnect.
package health

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Config controls the MQTT health reporter.
type Config struct {
	Broker          string        // e.g. tcp://127.0.0.1:1883
	Username        string        // MQTT username (broker requires auth)
	Password        string        // MQTT password
	ClientID        string        // MQTT client id
	BaseTopic       string        // state/availability topic root
	DiscoveryPrefix string        // Home Assistant discovery prefix
	WedgeAfter      time.Duration // sustained stream-start failure before flagging a problem

	// DeviceIdentifier, if set, attaches the sensors to an existing HA device
	// with this identifier (e.g. Scrypted's camera device) rather than creating a
	// dedicated per-camera device. Intended for single-camera setups, since one
	// id would otherwise merge all cameras into one device.
	DeviceIdentifier string
	// DeviceName is the device name sent alongside DeviceIdentifier. HA requires
	// a name on every entity that shares a device, and it must match the owning
	// integration's device name exactly (e.g. "Aqara Camera E1 (ONVIF)") to avoid
	// the two publishers overwriting each other's device name. Ignored unless
	// DeviceIdentifier is set; falls back to the camera name if empty.
	DeviceName string

	// Frame-drop reporting: flag "frame drops" once interval packet loss stays at
	// or above DropThreshold for DropWindow, and clear it once loss stays at or
	// below DropThreshold/2 for DropClearWindow (hysteresis to avoid flapping).
	DropThreshold   float64       // interval packet-loss fraction counted as high loss (e.g. 0.05)
	DropWindow      time.Duration // sustained high loss before flagging frame drops
	DropClearWindow time.Duration // sustained low loss before clearing
}

func (c *Config) applyDefaults() {
	if c.Broker == "" {
		c.Broker = "tcp://127.0.0.1:1883"
	}
	if c.ClientID == "" {
		c.ClientID = "homekit-rtsp-proxy"
	}
	if c.BaseTopic == "" {
		c.BaseTopic = "homekit-rtsp-proxy"
	}
	if c.DiscoveryPrefix == "" {
		c.DiscoveryPrefix = "homeassistant"
	}
	if c.WedgeAfter <= 0 {
		c.WedgeAfter = 90 * time.Second
	}
	if c.DropThreshold <= 0 {
		c.DropThreshold = 0.05
	}
	if c.DropWindow <= 0 {
		c.DropWindow = 20 * time.Second
	}
	if c.DropClearWindow <= 0 {
		c.DropClearWindow = 30 * time.Second
	}
}

// eventKind distinguishes the signals the streaming path sends to the reporter.
type eventKind int

const (
	evStreamResult eventKind = iota // a stream start attempt outcome (ok / errText)
	evStats                         // a periodic video-quality sample
)

// event is an internal signal from the streaming path.
type event struct {
	kind    eventKind
	camera  string
	ok      bool   // evStreamResult: true = started, false = failed
	errText string // evStreamResult: failure text

	// evStats: per-interval video counters.
	packets       uint64
	drops         uint64
	droppedFrames uint64
	interval      time.Duration
}

// camState is the per-camera health state. Guarded by Reporter.mu.
type camState struct {
	slug string
	name string

	// "Stream problem" sensor: camera cannot start a stream.
	problem   bool      // currently flagged as a problem to HA
	firstFail time.Time // start of the current unbroken failure streak
	lastFail  time.Time // most recent failure in the streak
	failCount int       // failures in the current streak
	since     time.Time // when the current problem/ok state began
	lastError string    // most recent stream-start error text

	// "Frame drops" sensor: stream runs but sheds packets/frames.
	degraded      bool      // currently flagged as frequent frame drops
	dropHighStart time.Time // start of the current sustained high-loss streak
	dropLowStart  time.Time // start of the current sustained low-loss streak
	dropSince     time.Time // when the current degraded/ok state began
	lastLoss      float64   // most recent interval packet-loss fraction
	lastDropped   uint64    // most recent interval dropped-frame count
	lastDropPub   time.Time // last drop-attributes publish (throttle)
}

// Reporter publishes per-camera stream health to Home Assistant over MQTT.
type Reporter struct {
	cfg         Config
	logger      *slog.Logger
	client      mqtt.Client
	statusTopic string

	mu   sync.Mutex
	cams map[string]*camState

	// now returns the current time; overridable in tests. Defaults to time.Now.
	now func() time.Time

	events chan event
	stop   chan struct{}
	wg     sync.WaitGroup
}

// staleGap resets the failure streak clock if attempts pause for this long, so
// a single failure hours after an unrelated blip does not instantly look wedged.
// A real wedge is hammered continuously by clients, so this is never hit then.
const staleGap = 3 * time.Minute

// New creates and connects a reporter for the given camera names. Connection is
// non-fatal: if the broker is unreachable the reporter retries in the
// background and returns without error.
func New(cfg Config, cameraNames []string, logger *slog.Logger) (*Reporter, error) {
	cfg.applyDefaults()
	if len(cameraNames) == 0 {
		return nil, fmt.Errorf("no cameras to report")
	}

	r := &Reporter{
		cfg:         cfg,
		logger:      logger,
		statusTopic: cfg.BaseTopic + "/status",
		cams:        make(map[string]*camState, len(cameraNames)),
		now:         time.Now,
		events:      make(chan event, 64),
		stop:        make(chan struct{}),
	}
	now := time.Now()
	for _, name := range cameraNames {
		r.cams[name] = &camState{slug: slugify(name), name: name, since: now, dropSince: now}
	}

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetKeepAlive(30*time.Second).
		SetConnectTimeout(10*time.Second).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10*time.Second).
		SetWill(r.statusTopic, "offline", 1, true).
		SetOnConnectHandler(func(mqtt.Client) { r.onConnect() }).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			r.logger.Warn("MQTT connection lost", "error", err)
		})
	r.client = mqtt.NewClient(opts)

	// Do not block startup on the broker. OnConnect publishes discovery and
	// state once connected; ConnectRetry keeps trying in the background.
	if token := r.client.Connect(); token.WaitTimeout(5*time.Second) && token.Error() != nil {
		r.logger.Warn("MQTT initial connect failed, retrying in background", "error", token.Error())
	}

	r.wg.Add(1)
	go r.run()
	return r, nil
}

// ReportStreamStarted records that a camera stream started successfully.
// Non-blocking and safe to call from the streaming path.
func (r *Reporter) ReportStreamStarted(camera string) {
	if r == nil {
		return
	}
	r.enqueue(event{kind: evStreamResult, camera: camera, ok: true})
}

// ReportStreamFailed records that a camera stream failed to start.
// Non-blocking and safe to call from the streaming path.
func (r *Reporter) ReportStreamFailed(camera, errText string) {
	if r == nil {
		return
	}
	r.enqueue(event{kind: evStreamResult, camera: camera, ok: false, errText: errText})
}

// ReportVideoStats records one periodic video-quality sample (per-interval
// counters). Non-blocking and safe to call from the streaming path.
func (r *Reporter) ReportVideoStats(camera string, packets, drops, droppedFrames uint64, interval time.Duration) {
	if r == nil {
		return
	}
	r.enqueue(event{
		kind:          evStats,
		camera:        camera,
		packets:       packets,
		drops:         drops,
		droppedFrames: droppedFrames,
		interval:      interval,
	})
}

func (r *Reporter) enqueue(e event) {
	select {
	case r.events <- e:
	default:
		// Buffer full (failure storm): drop. The state machine already has
		// plenty of signal from the events that did land.
	}
}

// Close marks the reporter offline and disconnects.
func (r *Reporter) Close() {
	if r == nil {
		return
	}
	close(r.stop)
	r.wg.Wait()
	if r.client.IsConnectionOpen() {
		// Explicit offline in addition to the LWT, for a clean shutdown.
		r.client.Publish(r.statusTopic, 1, true, "offline").WaitTimeout(time.Second)
	}
	r.client.Disconnect(250)
}

func (r *Reporter) run() {
	defer r.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case e := <-r.events:
			r.handleEvent(e)
		case <-ticker.C:
			r.handleTick()
		}
	}
}

func (r *Reporter) handleEvent(e event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	c := r.cams[e.camera]
	if c == nil {
		return
	}
	now := r.now()

	switch e.kind {
	case evStats:
		r.handleStats(c, e, now)
	default: // evStreamResult
		if stateChanged := r.applyEvent(c, e, now); stateChanged {
			if c.problem {
				r.logger.Warn("camera stream problem flagged to HA",
					"camera", c.name, "failures", c.failCount, "last_error", c.lastError)
			} else {
				r.logger.Info("camera stream recovered", "camera", c.name)
			}
			r.publishState(c)
		}
		r.publishAttributes(c)
		// A fresh successful start begins a new session: clear any stale
		// frame-drop state so it does not carry across sessions.
		if e.ok && r.resetDrop(c, now) {
			r.publishDropState(c)
		}
	}
}

// handleStats folds one video-quality sample into the frame-drop state and
// publishes as needed. Caller holds mu.
func (r *Reporter) handleStats(c *camState, e event, now time.Time) {
	loss := lossFraction(e.packets, e.drops)
	changed := r.applyStats(c, loss, e.droppedFrames, now)
	if changed {
		if c.degraded {
			r.logger.Warn("camera frame drops flagged to HA",
				"camera", c.name, "loss_pct", pct(loss), "dropped_frames", e.droppedFrames)
		} else {
			r.logger.Info("camera frame drops cleared", "camera", c.name)
		}
		r.publishDropState(c)
	}
	// Publish attributes on any change, otherwise throttled, so retained
	// loss figures stay fresh without a message every second.
	if changed || now.Sub(c.lastDropPub) >= 15*time.Second {
		r.publishDropAttributes(c)
		c.lastDropPub = now
	}
}

func (r *Reporter) handleTick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, c := range r.cams {
		if c.firstFail.IsZero() {
			continue
		}
		if r.promote(c, now) {
			r.logger.Warn("camera stream problem flagged to HA",
				"camera", c.name, "failures", c.failCount, "last_error", c.lastError)
			r.publishState(c)
		}
	}
}

// applyEvent updates c for one stream event and reports whether the problem
// state changed (so the state topic needs republishing). Pure state logic with
// no I/O, so it is unit-testable with an injected clock. Caller holds mu.
func (r *Reporter) applyEvent(c *camState, e event, now time.Time) (stateChanged bool) {
	if e.ok {
		c.firstFail = time.Time{}
		c.lastFail = time.Time{}
		c.failCount = 0
		c.lastError = ""
		if c.problem {
			c.problem = false
			c.since = now
			return true
		}
		return false
	}

	// Failure: extend the current streak, or start a fresh one if attempts
	// paused long enough that the old streak is no longer meaningful.
	if c.firstFail.IsZero() || now.Sub(c.lastFail) > staleGap {
		c.firstFail = now
		c.failCount = 0
	}
	c.lastFail = now
	c.failCount++
	c.lastError = e.errText
	return r.promote(c, now)
}

// promote flips c to problem if its failure streak has lasted at least
// WedgeAfter. Returns whether the state changed. Caller holds mu.
func (r *Reporter) promote(c *camState, now time.Time) (stateChanged bool) {
	if c.problem || c.firstFail.IsZero() {
		return false
	}
	if now.Sub(c.firstFail) >= r.cfg.WedgeAfter {
		c.problem = true
		c.since = now
		return true
	}
	return false
}

// applyStats folds one interval's packet-loss fraction into the frame-drop state
// with hysteresis and reports whether the degraded state changed. Pure state
// logic with no I/O, so it is unit-testable with an injected clock. Caller holds mu.
func (r *Reporter) applyStats(c *camState, loss float64, droppedFrames uint64, now time.Time) (stateChanged bool) {
	c.lastLoss = loss
	c.lastDropped = droppedFrames

	onT := r.cfg.DropThreshold
	clearT := onT / 2

	switch {
	case loss >= onT:
		if c.dropHighStart.IsZero() {
			c.dropHighStart = now
		}
		c.dropLowStart = time.Time{}
		if !c.degraded && now.Sub(c.dropHighStart) >= r.cfg.DropWindow {
			c.degraded = true
			c.dropSince = now
			return true
		}
	case loss <= clearT:
		if c.dropLowStart.IsZero() {
			c.dropLowStart = now
		}
		c.dropHighStart = time.Time{}
		if c.degraded && now.Sub(c.dropLowStart) >= r.cfg.DropClearWindow {
			c.degraded = false
			c.dropSince = now
			return true
		}
	default:
		// In the hysteresis dead band: neither streak makes progress, so a
		// transition requires a fresh sustained period on one side.
		c.dropHighStart = time.Time{}
		c.dropLowStart = time.Time{}
	}
	return false
}

// resetDrop clears frame-drop state at the start of a fresh stream session.
// Returns whether the degraded state changed. Caller holds mu.
func (r *Reporter) resetDrop(c *camState, now time.Time) (stateChanged bool) {
	c.dropHighStart = time.Time{}
	c.dropLowStart = time.Time{}
	c.lastLoss = 0
	c.lastDropped = 0
	if c.degraded {
		c.degraded = false
		c.dropSince = now
		return true
	}
	return false
}

// lossFraction is the fraction of expected packets missing over an interval.
func lossFraction(packets, drops uint64) float64 {
	total := packets + drops
	if total == 0 {
		return 0
	}
	return float64(drops) / float64(total)
}

// pct renders a 0..1 fraction as a percentage rounded to one decimal place.
func pct(frac float64) float64 {
	return math.Round(frac*1000) / 10
}

// onConnect (re)publishes discovery, availability and current state. Runs in the
// paho callback goroutine, so it takes the lock like any other state reader.
func (r *Reporter) onConnect() {
	r.logger.Info("MQTT connected", "broker", r.cfg.Broker)
	r.client.Publish(r.statusTopic, 1, true, "online")
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cams {
		r.publishDiscovery(c)
		r.publishState(c)
		r.publishAttributes(c)
		r.publishDropDiscovery(c)
		r.publishDropState(c)
		r.publishDropAttributes(c)
	}
}

// The publish* helpers assume the caller holds mu (they read camState fields).
// Publishes are fire-and-forget: paho queues them, and onConnect republishes
// retained state after any reconnect.

// deviceInfo returns the MQTT-discovery device block. When DeviceIdentifier is
// set, sensors attach to that pre-existing device (e.g. Scrypted's) by id alone
// so we don't override its name/model/manufacturer; otherwise the proxy owns a
// dedicated device per camera.
func (r *Reporter) deviceInfo(c *camState) map[string]any {
	if r.cfg.DeviceIdentifier != "" {
		// HA requires a name on every entity sharing a device; match the owning
		// integration's name so the two publishers don't overwrite each other.
		name := r.cfg.DeviceName
		if name == "" {
			name = c.name
		}
		return map[string]any{
			"identifiers": []string{r.cfg.DeviceIdentifier},
			"name":        name,
		}
	}
	return map[string]any{
		"identifiers":  []string{"homekit-rtsp-proxy-" + c.slug},
		"name":         c.name,
		"manufacturer": "homekit-rtsp-proxy",
		"model":        "HomeKit RTSP Proxy",
	}
}

func (r *Reporter) publishDiscovery(c *camState) {
	node := "homekit_rtsp_proxy_" + c.slug
	cfg := map[string]any{
		"name":                  "Stream problem",
		"unique_id":             node + "_stream_problem",
		"device_class":          "problem",
		"state_topic":           r.stateTopic(c),
		"payload_on":            "problem",
		"payload_off":           "ok",
		"json_attributes_topic": r.attrTopic(c),
		"availability": []map[string]string{{
			"topic":                 r.statusTopic,
			"payload_available":     "online",
			"payload_not_available": "offline",
		}},
		"device": r.deviceInfo(c),
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return
	}
	topic := fmt.Sprintf("%s/binary_sensor/%s/stream_problem/config", r.cfg.DiscoveryPrefix, node)
	r.client.Publish(topic, 1, true, payload)
}

func (r *Reporter) publishState(c *camState) {
	state := "ok"
	if c.problem {
		state = "problem"
	}
	r.client.Publish(r.stateTopic(c), 1, true, state)
}

func (r *Reporter) publishAttributes(c *camState) {
	attrs := map[string]any{
		"consecutive_failures": c.failCount,
		"since":                c.since.UTC().Format(time.RFC3339),
		"wedge_suspected":      isSetupEndpointsWedge(c.lastError),
	}
	if c.lastError != "" {
		attrs["last_error"] = c.lastError
	}
	payload, err := json.Marshal(attrs)
	if err != nil {
		return
	}
	r.client.Publish(r.attrTopic(c), 1, true, payload)
}

func (r *Reporter) stateTopic(c *camState) string {
	return fmt.Sprintf("%s/%s/stream/state", r.cfg.BaseTopic, c.slug)
}

func (r *Reporter) attrTopic(c *camState) string {
	return fmt.Sprintf("%s/%s/stream/attributes", r.cfg.BaseTopic, c.slug)
}

// --- "Frame drops" sensor ---

func (r *Reporter) publishDropDiscovery(c *camState) {
	node := "homekit_rtsp_proxy_" + c.slug
	cfg := map[string]any{
		"name":                  "Frame drops",
		"unique_id":             node + "_frame_drops",
		"device_class":          "problem",
		"state_topic":           r.dropStateTopic(c),
		"payload_on":            "problem",
		"payload_off":           "ok",
		"json_attributes_topic": r.dropAttrTopic(c),
		"availability": []map[string]string{{
			"topic":                 r.statusTopic,
			"payload_available":     "online",
			"payload_not_available": "offline",
		}},
		"device": r.deviceInfo(c),
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return
	}
	topic := fmt.Sprintf("%s/binary_sensor/%s/frame_drops/config", r.cfg.DiscoveryPrefix, node)
	r.client.Publish(topic, 1, true, payload)
}

func (r *Reporter) publishDropState(c *camState) {
	state := "ok"
	if c.degraded {
		state = "problem"
	}
	r.client.Publish(r.dropStateTopic(c), 1, true, state)
}

func (r *Reporter) publishDropAttributes(c *camState) {
	attrs := map[string]any{
		"loss_pct":            pct(c.lastLoss),
		"dropped_frames_last": c.lastDropped,
		"since":               c.dropSince.UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(attrs)
	if err != nil {
		return
	}
	r.client.Publish(r.dropAttrTopic(c), 1, true, payload)
}

func (r *Reporter) dropStateTopic(c *camState) string {
	return fmt.Sprintf("%s/%s/frames/state", r.cfg.BaseTopic, c.slug)
}

func (r *Reporter) dropAttrTopic(c *camState) string {
	return fmt.Sprintf("%s/%s/frames/attributes", r.cfg.BaseTopic, c.slug)
}

// isSetupEndpointsWedge reports whether an error text is the known camera-side
// wedge (SetupEndpoints returning HTTP 204), as opposed to a generic outage.
func isSetupEndpointsWedge(errText string) bool {
	return strings.Contains(errText, "SetupEndpoints") && strings.Contains(errText, "204")
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
