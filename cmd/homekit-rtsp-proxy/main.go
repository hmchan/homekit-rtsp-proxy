package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hmchan/homekit-rtsp-proxy/internal/config"
	"github.com/hmchan/homekit-rtsp-proxy/internal/hap"
	"github.com/hmchan/homekit-rtsp-proxy/internal/onvif"
	"github.com/hmchan/homekit-rtsp-proxy/internal/stream"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	unpair := flag.Bool("unpair", false, "remove all pairing data and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *unpair {
		os.RemoveAll("./.hkontroller")
		os.Remove(cfg.PairingStore)
		fmt.Println("pairing data removed")
		os.Exit(0)
	}

	// Set up structured logging.
	logLevel := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	// Load pairing store.
	store, err := hap.NewPairingStore(cfg.PairingStore)
	if err != nil {
		logger.Error("failed to load pairing store", "error", err)
		os.Exit(1)
	}

	// Determine bind address.
	bindAddr := cfg.BindAddress
	if bindAddr == "" {
		bindAddr = detectLocalIP()
	}
	logger.Info("using bind address", "address", bindAddr)

	// Create HAP controller.
	controller := hap.NewController(store, bindAddr, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := controller.Start(ctx); err != nil {
		logger.Error("failed to start HAP controller", "error", err)
		os.Exit(1)
	}

	// Wait for mDNS discovery, then pair with each camera.
	// Retry with increasing wait time to handle slow mDNS.
	logger.Info("waiting for mDNS discovery...")
	time.Sleep(10 * time.Second)

	for _, cam := range cfg.Cameras {
		camName := cam.Name
		camLogger := logger.With("camera", camName)

		camLogger.Info("connecting to camera", "setup_code", cam.SetupCode)

		var pairErr error
		for attempt := 1; attempt <= 6; attempt++ {
			pairErr = controller.PairCamera(ctx, camName, cam.SetupCode)
			if pairErr == nil {
				break
			}
			camLogger.Warn("pair attempt failed, retrying", "attempt", attempt, "error", pairErr)
			time.Sleep(10 * time.Second)
		}
		if pairErr != nil {
			camLogger.Error("failed to pair with camera", "error", pairErr)
			camLogger.Error("is the camera on the network? Is the setup code correct?")
			os.Exit(1)
		}
		camLogger.Info("camera paired and connected")
	}

	type cameraServices struct {
		name       string
		rtspServer *stream.RTSPServer
		onvifSrv   *onvif.Server
		srtpProxy  *stream.SRTPProxy
		session    *stream.Session
	}

	var services []cameraServices

	for _, cam := range cfg.Cameras {
		cam := cam
		camLogger := logger.With("camera", cam.Name)

		// Create SRTP proxy for this camera.
		srtpProxy := stream.NewSRTPProxy(camLogger)

		// Declare rtspServer early so the onStart closure can reference it.
		// It will be assigned after the session is created (closures capture by reference).
		var rtspServer *stream.RTSPServer

		// Create on-demand session. We declare it first so closures can reference it.
		localIP := net.ParseIP(bindAddr)
		var session *stream.Session
		session = stream.NewSession(cam.Name, cam.RTSP.IdleTimeout, camLogger,
			// onStart: called when first RTSP client connects.
			func() error {
				camLogger.Info("starting camera stream")
				rtspServer.ResetVideoRTP()

				// Open UDP ports first, so we know the actual ports for SetupEndpoints.
				videoPort, audioPort, err := srtpProxy.OpenPorts(0, 0)
				if err != nil {
					return fmt.Errorf("open SRTP ports: %w", err)
				}
				camLogger.Info("SRTP ports opened", "video", videoPort, "audio", audioPort)

				videoConfig := hap.VideoSelection{
					Profile:      hap.H264ProfileMain,
					Level:        hap.H264Level4_0,
					Width:        uint16(cam.Video.Width),
					Height:       uint16(cam.Video.Height),
					FPS:          cam.Video.FPS,
					MaxBitrate:   cam.Video.MaxBitrate,
					PayloadType:  99,
					RTCPInterval: 0.5,
				}

				audioConfig := hap.AudioSelection{
					BitRateMode:  0x00, // Variable
					PacketTime:   30,
					MaxBitrate:   24,
					PayloadType:  110,
					RTCPInterval: 5.0,
				}
				switch cam.Audio.Codec {
				case "opus":
					audioConfig.CodecType = hap.AudioCodecOpus
					audioConfig.SampleRate = hap.AudioSampleRate24kHz
				default: // "aac-eld"
					audioConfig.CodecType = hap.AudioCodecAACELD
					audioConfig.SampleRate = hap.AudioSampleRate16kHz
				}

				// Request stream from camera using the actual ports.
				resp, err := controller.StartStream(ctx, cam.Name, localIP,
					uint16(videoPort), uint16(audioPort),
					videoConfig, audioConfig)
				if err != nil {
					srtpProxy.Close()
					return fmt.Errorf("start HAP stream: %w", err)
				}

				// Start SRTP decryption with camera's keys.
				srtpCfg := stream.SRTPConfig{
					VideoKey:  resp.VideoSRTPKey,
					VideoSalt: resp.VideoSRTPSalt,
					AudioKey:  resp.AudioSRTPKey,
					AudioSalt: resp.AudioSRTPSalt,
					VideoSSRC: resp.VideoSSRC,
					AudioSSRC: resp.AudioSSRC,
					CameraAddr: &net.UDPAddr{
						IP:   resp.RemoteIP,
						Port: int(resp.RemoteVideoPort),
					},
					CameraAudioAddr: &net.UDPAddr{
						IP:   resp.RemoteIP,
						Port: int(resp.RemoteAudioPort),
					},
					ControllerVideoKey:   resp.ControllerVideoKey,
					ControllerVideoSalt:  resp.ControllerVideoSalt,
					ControllerVideoSSRC:  resp.ControllerVideoSSRC,
					ControllerAudioKey:   resp.ControllerAudioKey,
					ControllerAudioSalt:  resp.ControllerAudioSalt,
					ControllerAudioSSRC:  resp.ControllerAudioSSRC,
				}

				return srtpProxy.Start(srtpCfg)
			},
			// onStop: called when last RTSP client disconnects.
			func() error {
				camLogger.Info("stopping camera stream")
				srtpProxy.Close()
				// Best-effort: tell camera to stop. Errors are non-fatal
				// (camera may have already ended the session).
				if err := controller.StopStream(ctx, cam.Name, session.GetSessionID()); err != nil {
					camLogger.Warn("StopStream error (non-fatal)", "error", err)
				}
				return nil
			},
		)

		// Create RTSP server.
		rtspServer = stream.NewRTSPServer(stream.RTSPServerConfig{
			ListenAddress: cfg.ListenAddress,
			Port:          cam.RTSP.Port,
			Path:          cam.RTSP.Path,
			HasAudio:      cam.Audio.Enabled,
			AudioCodec:    cam.Audio.Codec,
			SampleRate:    cam.Audio.SampleRate,
			AudioGain:     *cam.Audio.Gain,
		}, session, camLogger)

		// Wire SRTP proxy output to RTSP server.
		srtpProxy.SetCallbacks(rtspServer.WriteVideoPacket, rtspServer.WriteAudioPacket)

		if err := rtspServer.Start(); err != nil {
			camLogger.Error("failed to start RTSP server", "error", err)
			os.Exit(1)
		}

		// Advertise the address consumers actually reach us on. When
		// ListenAddress is set (e.g. 127.0.0.1) it overrides bindAddr,
		// which is reserved for the camera-side SRTP path.
		advertiseAddr := bindAddr
		if cfg.ListenAddress != "" {
			advertiseAddr = cfg.ListenAddress
		}
		rtspURL := fmt.Sprintf("rtsp://%s:%d%s", advertiseAddr, cam.RTSP.Port, cam.RTSP.Path)
		camLogger.Info("RTSP URL available", "url", rtspURL)

		svc := cameraServices{
			name:       cam.Name,
			rtspServer: rtspServer,
			srtpProxy:  srtpProxy,
			session:    session,
		}

		// Set up ONVIF server if enabled.
		if cam.ONVIF.Enabled {
			hostAddr := fmt.Sprintf("%s:%d", advertiseAddr, cam.ONVIF.Port)
			onvifSrv := onvif.NewServer(onvif.ServerConfig{
				ListenAddress: cfg.ListenAddress,
				Port:          cam.ONVIF.Port,
				HostAddr:      hostAddr,
				RTSPURL:       rtspURL,
				CameraName:    cam.Name,
				VideoWidth:    cam.Video.Width,
				VideoHeight:   cam.Video.Height,
				VideoFPS:      cam.Video.FPS,
				VideoBitrate:  cam.Video.MaxBitrate,
			}, camLogger)

			if err := onvifSrv.Start(); err != nil {
				camLogger.Error("failed to start ONVIF server", "error", err)
				os.Exit(1)
			}

			// Subscribe to HAP motion events and relay to ONVIF.
			err := controller.SubscribeMotionSensor(ctx, cam.Name, func(detected bool) {
				camLogger.Info("motion event", "detected", detected)
				onvifSrv.NotifyMotion(detected)
			})
			if err != nil {
				camLogger.Warn("failed to subscribe to motion sensor (camera may not have one)", "error", err)
			}

			svc.onvifSrv = onvifSrv
		}

		services = append(services, svc)
	}

	// Restart any active stream after the controller auto-recovers a
	// camera (e.g. after a camera reboot). The reconnect itself only
	// re-pair-verifies and re-subscribes motion; SRTP needs a fresh
	// SetupEndpoints to resume packet flow.
	controller.SetRecoveredCallback(func(deviceName string) {
		for _, svc := range services {
			if svc.name != deviceName {
				continue
			}
			if err := svc.session.Restart(); err != nil {
				logger.Error("session restart after recovery failed",
					"camera", deviceName, "error", err)
			}
			return
		}
	})

	logger.Info("all cameras ready, waiting for RTSP clients")

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	// Graceful shutdown.
	for _, svc := range services {
		if svc.onvifSrv != nil {
			svc.onvifSrv.Stop()
		}
		// Stop the camera stream cleanly first (cancels any warm-mode timer
		// and tells the camera to end the HAP session) before tearing down
		// the SRTP proxy that the onStop callback uses.
		if err := svc.session.Shutdown(); err != nil {
			logger.Warn("session shutdown error", "error", err)
		}
		svc.rtspServer.Stop()
		svc.srtpProxy.Close()
	}

	controller.Stop()
	logger.Info("shutdown complete")
}

// detectLocalIP finds the primary outbound IP address.
func detectLocalIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return "0.0.0.0"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
