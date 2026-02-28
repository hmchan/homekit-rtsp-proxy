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

// Server implements an ONVIF-compatible HTTP server that provides
// device, media, and event services for a single camera.
type Server struct {
	logger     *slog.Logger
	port       int
	hostAddr   string // host:port for self-referencing URLs
	rtspURL    string
	cameraName string
	videoWidth int
	videoHeight int
	videoFPS   int
	videoBitrate int

	pullpoints *PullPointManager
	httpServer *http.Server
}

// ServerConfig configures the ONVIF server.
type ServerConfig struct {
	Port        int
	HostAddr    string // e.g., "192.168.1.10:8580"
	RTSPURL     string // e.g., "rtsp://192.168.1.10:8554/live"
	CameraName  string
	VideoWidth  int
	VideoHeight int
	VideoFPS    int
	VideoBitrate int
}

func NewServer(cfg ServerConfig, logger *slog.Logger) *Server {
	return &Server{
		logger:       logger,
		port:         cfg.Port,
		hostAddr:     cfg.HostAddr,
		rtspURL:      cfg.RTSPURL,
		cameraName:   cfg.CameraName,
		videoWidth:   cfg.VideoWidth,
		videoHeight:  cfg.VideoHeight,
		videoFPS:     cfg.VideoFPS,
		videoBitrate: cfg.VideoBitrate,
		pullpoints:   NewPullPointManager(),
	}
}

// Start begins the ONVIF HTTP server.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/onvif/device_service", s.handleDeviceService)
	mux.HandleFunc("/onvif/media_service", s.handleMediaService)
	mux.HandleFunc("/onvif/event_service", s.handleEventService)
	mux.HandleFunc("/onvif/event_service/pullpoint/", s.handlePullPoint)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
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

func (s *Server) readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(r.Body)
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
	body, err := s.readBody(r)
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
	body, err := s.readBody(r)
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
		s.writeSOAP(w, `<trt:GetSnapshotUriResponse><trt:MediaUri><tt:Uri></tt:Uri></trt:MediaUri></trt:GetSnapshotUriResponse>`)

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
	body, err := s.readBody(r)
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

	body, err := s.readBody(r)
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
