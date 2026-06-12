package onvif

import (
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SnapshotProvider returns the most recent JPEG-encoded snapshot for a
// camera, or nil if none is available yet.
type SnapshotProvider interface {
	LatestSnapshotJPEG() []byte
}

// Server implements an ONVIF-compatible HTTP server that provides
// device, media, and event services for a single camera.
type Server struct {
	logger       *slog.Logger
	listenAddr   string
	port         int
	hostAddr     string // host:port for self-referencing URLs
	rtspURL      string
	snapshotURL  string
	cameraName   string
	videoWidth   int
	videoHeight  int
	videoFPS     int
	videoBitrate int

	pullpoints *PullPointManager
	httpServer *http.Server
	snapshots  SnapshotProvider // optional; nil disables /snapshot.jpg
}

// ServerConfig configures the ONVIF server.
type ServerConfig struct {
	ListenAddress string // "" = all interfaces, "127.0.0.1" = local only
	Port          int
	HostAddr      string // e.g., "192.168.1.10:8580"
	RTSPURL       string // e.g., "rtsp://192.168.1.10:8554/live"
	CameraName    string
	VideoWidth    int
	VideoHeight   int
	VideoFPS      int
	VideoBitrate  int
	// Snapshots, when non-nil, enables the /snapshot.jpg HTTP endpoint
	// and a non-empty GetSnapshotUri SOAP response.
	Snapshots SnapshotProvider
}

func NewServer(cfg ServerConfig, logger *slog.Logger) *Server {
	s := &Server{
		logger:       logger,
		listenAddr:   cfg.ListenAddress,
		port:         cfg.Port,
		hostAddr:     cfg.HostAddr,
		rtspURL:      cfg.RTSPURL,
		cameraName:   cfg.CameraName,
		videoWidth:   cfg.VideoWidth,
		videoHeight:  cfg.VideoHeight,
		videoFPS:     cfg.VideoFPS,
		videoBitrate: cfg.VideoBitrate,
		pullpoints:   NewPullPointManager(),
		snapshots:    cfg.Snapshots,
	}
	if cfg.Snapshots != nil {
		s.snapshotURL = fmt.Sprintf("http://%s/snapshot.jpg", cfg.HostAddr)
	}
	return s
}

// Start begins the ONVIF HTTP server.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/onvif/device_service", s.handleDeviceService)
	mux.HandleFunc("/onvif/media_service", s.handleMediaService)
	mux.HandleFunc("/onvif/event_service", s.handleEventService)
	mux.HandleFunc("/onvif/event_service/pullpoint/", s.handlePullPoint)
	if s.snapshots != nil {
		mux.HandleFunc("/snapshot.jpg", s.handleSnapshot)
	}

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.listenAddr, s.port),
		Handler: mux,
		// WriteTimeout must outlast the 30 s PullMessages long-poll.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      40 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start expired subscription cleanup.
	go s.cleanupLoop()

	s.logger.Info("ONVIF server started", "port", s.port)

	go func() {
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			s.logger.Error("ONVIF server error", "error", err)
		}
	}()

	return nil
}

// Stop shuts down the ONVIF server.
func (s *Server) Stop() {
	if s.httpServer != nil {
		s.httpServer.Close()
	}
}

// NotifyMotion sends a motion event to all PullPoint subscribers.
func (s *Server) NotifyMotion(isMotion bool) {
	s.pullpoints.FanOut(MotionEvent{
		Time:     time.Now(),
		IsMotion: isMotion,
	})
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.pullpoints.CleanExpired()
	}
}

// soapAction extracts the SOAP action from the request body.
type soapEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    soapBody `xml:"Body"`
}

type soapBody struct {
	Content []byte `xml:",innerxml"`
}

func extractAction(body []byte) string {
	// Simple extraction: look for the first element inside <Body>.
	content := string(body)
	for _, action := range []string{
		"GetDeviceInformation",
		"GetCapabilities",
		"GetServices",
		"GetProfiles",
		"GetStreamUri",
		"GetVideoSources",
		"GetVideoEncoderConfigurationOptions",
		"GetVideoEncoderConfiguration",
		"SetVideoEncoderConfiguration",
		"GetSnapshotUri",
		"GetAudioSources",
		"GetAudioEncoderConfigurationOptions",
		"GetEventProperties",
		"CreatePullPointSubscription",
		"PullMessages",
		"Renew",
		"Unsubscribe",
		"GetServiceCapabilities",
		"GetSystemDateAndTime",
		"GetScopes",
		"GetNetworkInterfaces",
	} {
		if strings.Contains(content, action) {
			return action
		}
	}
	return ""
}

// maxSOAPBody caps request bodies; real ONVIF SOAP requests are well under
// 64 KB, and an uncapped ReadAll lets any client exhaust memory.
const maxSOAPBody = 64 << 10

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSOAPBody))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (s *Server) writeSOAP(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	fmt.Fprintf(w, "%s%s%s", soapEnvelopeHeader, content, soapEnvelopeFooter)
}

func (s *Server) handleDeviceService(w http.ResponseWriter, r *http.Request) {
	body, err := s.readBody(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	action := extractAction(body)
	s.logger.Debug("ONVIF device service", "action", action)

	switch action {
	case "GetDeviceInformation":
		s.writeSOAP(w, fmt.Sprintf(getDeviceInformationResponse, s.cameraName, s.cameraName))

	case "GetCapabilities":
		s.writeSOAP(w, fmt.Sprintf(getCapabilitiesResponse, s.hostAddr, s.hostAddr, s.hostAddr))

	case "GetServices":
		s.writeSOAP(w, fmt.Sprintf(getServicesResponse, s.hostAddr, s.hostAddr, s.hostAddr))

	case "GetSystemDateAndTime":
		now := time.Now().UTC()
		resp := fmt.Sprintf(`
    <tds:GetSystemDateAndTimeResponse>
      <tds:SystemDateAndTime>
        <tt:DateTimeType>NTP</tt:DateTimeType>
        <tt:UTCDateTime>
          <tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>
          <tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>
        </tt:UTCDateTime>
      </tds:SystemDateAndTime>
    </tds:GetSystemDateAndTimeResponse>`,
			now.Hour(), now.Minute(), now.Second(),
			now.Year(), now.Month(), now.Day())
		s.writeSOAP(w, resp)

	case "GetScopes":
		resp := fmt.Sprintf(`
    <tds:GetScopesResponse>
      <tds:Scopes>
        <tt:ScopeDef>Fixed</tt:ScopeDef>
        <tt:ScopeItem>onvif://www.onvif.org/name/%s</tt:ScopeItem>
      </tds:Scopes>
      <tds:Scopes>
        <tt:ScopeDef>Fixed</tt:ScopeDef>
        <tt:ScopeItem>onvif://www.onvif.org/type/video_encoder</tt:ScopeItem>
      </tds:Scopes>
    </tds:GetScopesResponse>`, s.cameraName)
		s.writeSOAP(w, resp)

	default:
		s.writeSOAP(w, fmt.Sprintf(getDeviceInformationResponse, s.cameraName, s.cameraName))
	}
}

func (s *Server) handleMediaService(w http.ResponseWriter, r *http.Request) {
	body, err := s.readBody(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	action := extractAction(body)
	s.logger.Debug("ONVIF media service", "action", action)

	switch action {
	case "GetProfiles":
		s.writeSOAP(w, fmt.Sprintf(getProfilesResponse,
			s.videoWidth, s.videoHeight,
			s.videoWidth, s.videoHeight,
			s.videoFPS, s.videoBitrate))

	case "GetStreamUri":
		s.writeSOAP(w, fmt.Sprintf(getStreamUriResponse, s.rtspURL))

	case "GetVideoSources":
		s.writeSOAP(w, fmt.Sprintf(getVideoSourcesResponse, s.videoWidth, s.videoHeight))

	case "GetVideoEncoderConfiguration":
		s.writeSOAP(w, fmt.Sprintf(getVideoEncoderConfigurationResponse,
			s.videoWidth, s.videoHeight, s.videoFPS, s.videoBitrate))

	case "GetVideoEncoderConfigurationOptions":
		s.writeSOAP(w, fmt.Sprintf(getVideoEncoderConfigurationOptionsResponse,
			s.videoWidth, s.videoHeight, s.videoFPS, s.videoBitrate))

	case "SetVideoEncoderConfiguration":
		s.writeSOAP(w, `<trt:SetVideoEncoderConfigurationResponse/>`)

	case "GetSnapshotUri":
		s.writeSOAP(w, fmt.Sprintf(
			`<trt:GetSnapshotUriResponse><trt:MediaUri><tt:Uri>%s</tt:Uri></trt:MediaUri></trt:GetSnapshotUriResponse>`,
			s.snapshotURL))

	case "GetAudioSources":
		s.writeSOAP(w, `<trt:GetAudioSourcesResponse/>`)

	case "GetAudioEncoderConfigurationOptions":
		s.writeSOAP(w, `<trt:GetAudioEncoderConfigurationOptionsResponse/>`)

	case "GetServiceCapabilities":
		s.writeSOAP(w, `<trt:GetServiceCapabilitiesResponse><trt:Capabilities/></trt:GetServiceCapabilitiesResponse>`)

	default:
		s.logger.Debug("ONVIF media service unhandled", "action", action, "body", string(body))
		s.writeSOAP(w, fmt.Sprintf(getProfilesResponse,
			s.videoWidth, s.videoHeight,
			s.videoWidth, s.videoHeight,
			s.videoFPS, s.videoBitrate))
	}
}

func (s *Server) handleEventService(w http.ResponseWriter, r *http.Request) {
	body, err := s.readBody(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	action := extractAction(body)
	s.logger.Debug("ONVIF event service", "action", action)

	switch action {
	case "GetEventProperties":
		s.writeSOAP(w, getEventPropertiesResponse)

	case "CreatePullPointSubscription":
		subID := uuid.New().String()
		timeout := 60 * time.Second
		sub := s.pullpoints.Create(subID, timeout)
		if sub == nil {
			s.logger.Warn("pullpoint subscription limit reached, rejecting")
			http.Error(w, "too many subscriptions", http.StatusServiceUnavailable)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		termination := sub.TerminationTime.UTC().Format(time.RFC3339)

		s.writeSOAP(w, fmt.Sprintf(createPullPointSubscriptionResponse,
			s.hostAddr, subID, now, termination))

	case "GetServiceCapabilities":
		s.writeSOAP(w, `<tev:GetServiceCapabilitiesResponse><tev:Capabilities WSPullPointSupport="true"/></tev:GetServiceCapabilitiesResponse>`)

	default:
		s.writeSOAP(w, getEventPropertiesResponse)
	}
}

func (s *Server) handlePullPoint(w http.ResponseWriter, r *http.Request) {
	// Extract subscription ID from URL path.
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, "invalid pullpoint URL", http.StatusBadRequest)
		return
	}
	subID := parts[len(parts)-1]

	body, err := s.readBody(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	action := extractAction(body)
	s.logger.Debug("ONVIF pullpoint", "action", action, "subscription", subID)

	sub := s.pullpoints.Get(subID)
	if sub == nil {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}

	switch action {
	case "PullMessages":
		// Long-poll for events.
		events := sub.PullMessages(30 * time.Second)

		now := time.Now().UTC().Format(time.RFC3339)
		termination := sub.TerminationTime.UTC().Format(time.RFC3339)

		var messages string
		for _, evt := range events {
			isMotion := "false"
			if evt.IsMotion {
				isMotion = "true"
			}
			messages += fmt.Sprintf(notificationMessage,
				evt.Time.UTC().Format(time.RFC3339), isMotion)
		}

		s.writeSOAP(w, fmt.Sprintf(pullMessagesResponse, now, termination, messages))

	case "Renew":
		sub.Renew(60 * time.Second)
		now := time.Now().UTC().Format(time.RFC3339)
		termination := sub.TerminationTime.UTC().Format(time.RFC3339)
		s.writeSOAP(w, fmt.Sprintf(`
    <wsnt:RenewResponse>
      <wsnt:CurrentTime>%s</wsnt:CurrentTime>
      <wsnt:TerminationTime>%s</wsnt:TerminationTime>
    </wsnt:RenewResponse>`, now, termination))

	case "Unsubscribe":
		s.pullpoints.Remove(subID)
		s.writeSOAP(w, `<wsnt:UnsubscribeResponse/>`)

	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// handleSnapshot serves the latest cached JPEG snapshot, if any.
// Polls (e.g., from HA's Generic Camera) typically arrive at a few Hz;
// the JPEG itself is refreshed once per H.264 IDR (~every GOP) by the
// RTSP server's Snapshotter.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	jpeg := s.snapshots.LatestSnapshotJPEG()
	if len(jpeg) == 0 {
		http.Error(w, "no snapshot available yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jpeg)))
	w.Write(jpeg)
}
