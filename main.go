package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

var (
	host, apiKey, apiSecret, roomName, identity string
	livekitTrack                                *webrtc.TrackLocalStaticRTP
	embeddedTrack                               *lksdk.LocalTrack
	log                                         logger.Logger
)

type App struct {
	room       *lksdk.Room
	server     *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	peerConns  map[string]*webrtc.PeerConnection
	peerConnMu sync.RWMutex
}

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "embedded", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "livekit-embedded-bridge")
	log = logger.GetLogger()
	lksdk.SetLogger(log)
	
	flag.Parse()
	if err := validateFlags(); err != nil {
		log.Errorw("invalid arguments", err)
		os.Exit(1)
	}

	app := &App{
		peerConns: make(map[string]*webrtc.PeerConnection),
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	if err := app.initialize(); err != nil {
		log.Errorw("failed to initialize application", err)
		os.Exit(1)
	}

	// Start HTTP server in a goroutine
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		if err := app.startServer(); err != nil && err != http.ErrServerClosed {
			log.Errorw("HTTP server error", err)
		}
	}()

	log.Infow("Application started successfully", "port", "8080")

	// Wait for shutdown signal
	<-sigChan
	log.Infow("Shutdown signal received, starting graceful shutdown...")
	
	app.shutdown()
}

func validateFlags() error {
	if host == "" {
		return fmt.Errorf("host is required")
	}
	if apiKey == "" {
		return fmt.Errorf("api-key is required")
	}
	if apiSecret == "" {
		return fmt.Errorf("api-secret is required")
	}
	if roomName == "" {
		return fmt.Errorf("room-name is required")
	}
	if identity == "" {
		return fmt.Errorf("identity is required")
	}
	return nil
}

func (app *App) initialize() error {
	var err error
	
	// Create LiveKit track
	livekitTrack, err = webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "pion",
	)
	if err != nil {
		return fmt.Errorf("failed to create LiveKit track: %w", err)
	}

	// Generate access token
	token, err := newAccessToken(apiKey, apiSecret, roomName, identity)
	if err != nil {
		return fmt.Errorf("failed to create access token: %w", err)
	}

	// Create room with callbacks
	app.room = lksdk.NewRoom(&lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: app.onTrackSubscribed,
		},
	})

	// Prepare and join room
	if err := app.room.PrepareConnection(host, token); err != nil {
		return fmt.Errorf("failed to prepare room connection: %w", err)
	}

	if err := app.room.JoinWithToken(host, token); err != nil {
		return fmt.Errorf("failed to join room: %w", err)
	}

	// Create embedded track
	embeddedTrack, err = lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus})
	if err != nil {
		return fmt.Errorf("failed to create embedded track: %w", err)
	}

	// Publish track
	if _, err = app.room.LocalParticipant.PublishTrack(embeddedTrack, &lksdk.TrackPublicationOptions{
		Name: "embedded",
	}); err != nil {
		return fmt.Errorf("failed to publish track: %w", err)
	}

	return nil
}

func (app *App) onTrackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log.Infow("Track subscribed", "participant", rp.Identity(), "track", publication.Name())
	
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		defer log.Infow("Track reading goroutine terminated", "participant", rp.Identity())
		
		for {
			select {
			case <-app.ctx.Done():
				log.Infow("Context cancelled, stopping track reading", "participant", rp.Identity())
				return
			default:
				rtpPacket, _, rtpErr := track.ReadRTP()
				if rtpErr != nil {
					if rtpErr == io.EOF {
						log.Infow("Track ended", "participant", rp.Identity())
					} else {
						log.Errorw("Failed to read RTP packet", rtpErr, "participant", rp.Identity())
					}
					return
				}

				if rtpErr = livekitTrack.WriteRTP(rtpPacket); rtpErr != nil {
					log.Errorw("Failed to write RTP packet to LiveKit track", rtpErr)
					return
				}
			}
		}
	}()
}

func (app *App) startServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/connect", app.connectHandler)
	
	app.server = &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	
	log.Infow("Server listening on :8080")
	return app.server.ListenAndServe()
}

func (app *App) connectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		log.Errorw("Failed to read request body", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Errorw("Failed to create peer connection", err)
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}

	// Store peer connection for cleanup
	connID := fmt.Sprintf("%p", pc)
	app.peerConnMu.Lock()
	app.peerConns[connID] = pc
	app.peerConnMu.Unlock()

	// Setup track handler
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			log.Infow("Audio track received from peer connection")
			
			app.wg.Add(1)
			go func() {
				defer app.wg.Done()
				defer log.Infow("Peer connection track reading goroutine terminated")
				
				for {
					select {
					case <-app.ctx.Done():
						log.Infow("Context cancelled, stopping peer track reading")
						return
					default:
						rtpPacket, _, rtpErr := track.ReadRTP()
						if rtpErr != nil {
							if rtpErr == io.EOF {
								log.Infow("Peer track ended")
							} else {
								log.Errorw("Failed to read RTP packet from peer", rtpErr)
							}
							return
						}

						if rtpErr = embeddedTrack.WriteRTP(rtpPacket, nil); rtpErr != nil {
							log.Errorw("Failed to write RTP packet to embedded track", rtpErr)
							return
						}
					}
				}
			}()
		}
	})

	// Add track to peer connection
	if _, err = pc.AddTrack(livekitTrack); err != nil {
		log.Errorw("Failed to add track to peer connection", err)
		http.Error(w, "Failed to add track", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	}

	// Setup ICE connection state change handler
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Infow("ICE connection state changed", "state", state)
		if state == webrtc.ICEConnectionStateFailed || 
		   state == webrtc.ICEConnectionStateDisconnected ||
		   state == webrtc.ICEConnectionStateClosed {
			app.cleanupPeerConnection(connID)
		}
	})

	// Set remote description
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, 
		SDP:  string(offer),
	}); err != nil {
		log.Errorw("Failed to set remote description", err)
		http.Error(w, "Failed to set remote description", http.StatusBadRequest)
		app.cleanupPeerConnection(connID)
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Errorw("Failed to create answer", err)
		http.Error(w, "Failed to create answer", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	}

	// Set local description
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Errorw("Failed to set local description", err)
		http.Error(w, "Failed to set local description", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	}

	// Wait for ICE gathering to complete with timeout
	select {
	case <-webrtc.GatheringCompletePromise(pc):
		// ICE gathering completed
	case <-time.After(10 * time.Second):
		log.Infow("ICE gathering timeout")
		http.Error(w, "ICE gathering timeout", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	case <-app.ctx.Done():
		log.Infow("Context cancelled during ICE gathering")
		http.Error(w, "Server shutting down", http.StatusServiceUnavailable)
		app.cleanupPeerConnection(connID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	
	if _, err := fmt.Fprint(w, pc.LocalDescription().SDP); err != nil {
		log.Errorw("Failed to write response", err)
	}
	
	log.Infow("Successfully handled connect request")
}

func (app *App) cleanupPeerConnection(connID string) {
	app.peerConnMu.Lock()
	defer app.peerConnMu.Unlock()
	
	if pc, exists := app.peerConns[connID]; exists {
		if err := pc.Close(); err != nil {
			log.Errorw("Failed to close peer connection", err)
		}
		delete(app.peerConns, connID)
		log.Infow("Peer connection cleaned up", "connID", connID)
	}
}

func (app *App) shutdown() {
	log.Infow("Starting graceful shutdown...")
	
	// Cancel context to stop all goroutines
	app.cancel()
	
	// Shutdown HTTP server with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	
	if app.server != nil {
		if err := app.server.Shutdown(shutdownCtx); err != nil {
			log.Errorw("Failed to shutdown HTTP server gracefully", err)
		} else {
			log.Infow("HTTP server shutdown completed")
		}
	}
	
	// Close all peer connections
	app.peerConnMu.Lock()
	for connID, pc := range app.peerConns {
		if err := pc.Close(); err != nil {
			log.Errorw("Failed to close peer connection during shutdown", err, "connID", connID)
		}
	}
	app.peerConnMu.Unlock()
	log.Infow("All peer connections closed")
	
	// Close LiveKit room
	if app.room != nil {
		app.room.Disconnect()
		log.Infow("LiveKit room disconnected")
	}
	
	// Wait for all goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		log.Infow("All goroutines terminated")
	case <-time.After(15 * time.Second):
		log.Infow("Timeout waiting for goroutines to terminate")
	}
	
	log.Infow("Graceful shutdown completed")
}

func newAccessToken(apiKey, apiSecret, roomName, pID string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(pID).
		SetName(pID)

	return at.ToJWT()
}
