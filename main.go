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

	"github.com/joho/godotenv"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
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
	room         *lksdk.Room
	server       *http.Server
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	peerConns    map[string]*webrtc.PeerConnection
	peerConnMu   sync.RWMutex
	livekitReady bool
	livekitMu    sync.RWMutex
	
	// Room management
	roomClient     *lksdk.RoomServiceClient
	dispatchClient *lksdk.AgentDispatchClient
	roomConnections map[string]int  // roomName -> connection count
	roomConnMu     sync.RWMutex
	currentRoom    string          // currently connected room
}

func init() {
	flag.StringVar(&host, "host", os.Getenv("LIVEKIT_HOST"), "livekit server host")
	flag.StringVar(&apiKey, "api-key", os.Getenv("LIVEKIT_API_KEY"), "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", os.Getenv("LIVEKIT_API_SECRET"), "livekit api secret")
	flag.StringVar(&roomName, "room-name", "embedded", "room name")
	flag.StringVar(&identity, "identity", os.Getenv("IDENTITY"), "participant identity")
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "livekit-embedded-bridge")
	log = logger.GetLogger()
	lksdk.SetLogger(log)
	
	godotenv.Load(".env")
	flag.Parse()
	if err := validateFlags(); err != nil {
		log.Errorw("invalid arguments", err)
		os.Exit(1)
	}

	app := &App{
		peerConns:       make(map[string]*webrtc.PeerConnection),
		livekitReady:    false,
		roomConnections: make(map[string]int),
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in a goroutine
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		if err := app.startServer(); err != nil && err != http.ErrServerClosed {
			log.Errorw("HTTP server error", err)
		}
	}()

	log.Infow("Application started successfully", "port", "8081", "note", "LiveKit will connect when first microcontroller connects")

	// Wait for shutdown signal
	<-sigChan
	log.Infow("Shutdown signal received, starting graceful shutdown...")
	
	app.shutdown()
}

func validateFlags() error {
	// Read directly from environment variables, with fallbacks to flag values
	envHost := os.Getenv("LIVEKIT_HOST")
	if envHost == "" {
		envHost = host
	}
	if envHost == "" {
		return fmt.Errorf("LIVEKIT_HOST environment variable or -host flag is required")
	}

	envApiKey := os.Getenv("LIVEKIT_API_KEY")
	if envApiKey == "" {
		envApiKey = apiKey
	}
	if envApiKey == "" {
		return fmt.Errorf("LIVEKIT_API_KEY environment variable or -api-key flag is required")
	}

	envApiSecret := os.Getenv("LIVEKIT_API_SECRET")
	if envApiSecret == "" {
		envApiSecret = apiSecret
	}
	if envApiSecret == "" {
		return fmt.Errorf("LIVEKIT_API_SECRET environment variable or -api-secret flag is required")
	}

	envIdentity := os.Getenv("IDENTITY")
	if envIdentity == "" {
		envIdentity = identity
	}
	if envIdentity == "" {
		return fmt.Errorf("IDENTITY environment variable or -identity flag is required")
	}

	// Update the global variables with the final values
	host = envHost
	apiKey = envApiKey
	apiSecret = envApiSecret
	identity = envIdentity

	return nil
}

func (app *App) initializeLiveKitForRoom(roomName string) error {
	app.livekitMu.Lock()
	defer app.livekitMu.Unlock()
	
	// Initialize clients if not already done
	if app.roomClient == nil {
		app.roomClient = lksdk.NewRoomServiceClient(host, apiKey, apiSecret)
		app.dispatchClient = lksdk.NewAgentDispatchServiceClient(host, apiKey, apiSecret)
		log.Infow("Room service and agent dispatch clients initialized")
	}
	
	// Check if already initialized for this room
	if app.livekitReady && app.currentRoom == roomName {
		// Increment connection count for this room
		app.roomConnMu.Lock()
		app.roomConnections[roomName]++
		app.roomConnMu.Unlock()
		log.Infow("Reusing existing LiveKit connection", "roomName", roomName, "connections", app.roomConnections[roomName])
		return nil
	}
	
	// If we're connected to a different room, disconnect first
	if app.livekitReady && app.currentRoom != roomName {
		log.Infow("Switching rooms, disconnecting from current room", "currentRoom", app.currentRoom, "newRoom", roomName)
		if app.room != nil {
			app.room.Disconnect()
		}
		app.livekitReady = false
	}
	
	log.Infow("Initializing LiveKit connection...", "roomName", roomName)
	
	var err error
	
	// Create LiveKit track
	livekitTrack, err = webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "pion",
	)
	if err != nil {
		return fmt.Errorf("failed to create LiveKit track: %w", err)
	}

	// Generate access token with dynamic room name
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

	app.livekitReady = true
	app.currentRoom = roomName
	
	// Initialize connection count for this room
	app.roomConnMu.Lock()
	app.roomConnections[roomName] = 1
	app.roomConnMu.Unlock()
	
	log.Infow("LiveKit connection established successfully", "roomName", roomName, "connections", 1)
	
	// Dispatch the okchat-voice-agent to the room
	go app.dispatchAgent(roomName)
	
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
		Addr:    ":8081",
		Handler: mux,
	}
	
	log.Infow("Server listening on :8081")
	return app.server.ListenAndServe()
}

func (app *App) connectHandler(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	
	// Handle preflight OPTIONS request
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get chatbotId from query parameters
	chatbotId := r.URL.Query().Get("chatbotId")
	if chatbotId == "" {
		log.Errorw("Missing chatbotId query parameter", fmt.Errorf("chatbotId parameter is required"))
		http.Error(w, "Missing required query parameter: chatbotId", http.StatusBadRequest)
		return
	}

	// Create room name in format "esp-{chatbotId}"
	dynamicRoomName := fmt.Sprintf("esp-%s", chatbotId)
	log.Infow("Processing connection request", "chatbotId", chatbotId, "roomName", dynamicRoomName)

	// Initialize LiveKit connection if not already done
	if err := app.initializeLiveKitForRoom(dynamicRoomName); err != nil {
		log.Errorw("Failed to initialize LiveKit", err, "roomName", dynamicRoomName)
		http.Error(w, "Failed to initialize LiveKit connection", http.StatusInternalServerError)
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
			log.Infow("Audio track received from peer connection", "chatbotId", chatbotId)
			
			app.wg.Add(1)
			go func() {
				defer app.wg.Done()
				defer log.Infow("Peer connection track reading goroutine terminated", "chatbotId", chatbotId)
				
				for {
					select {
					case <-app.ctx.Done():
						log.Infow("Context cancelled, stopping peer track reading", "chatbotId", chatbotId)
						return
					default:
						rtpPacket, _, rtpErr := track.ReadRTP()
						if rtpErr != nil {
							if rtpErr == io.EOF {
								log.Infow("Peer track ended", "chatbotId", chatbotId)
							} else {
								log.Errorw("Failed to read RTP packet from peer", rtpErr, "chatbotId", chatbotId)
							}
							return
						}

						if rtpErr = embeddedTrack.WriteRTP(rtpPacket, nil); rtpErr != nil {
							log.Errorw("Failed to write RTP packet to embedded track", rtpErr, "chatbotId", chatbotId)
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
		log.Infow("ICE connection state changed", "state", state, "chatbotId", chatbotId)
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
	
	log.Infow("Successfully handled connect request", "chatbotId", chatbotId, "roomName", dynamicRoomName)
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
		
		// Handle room cleanup when connection is closed
		go app.handleRoomCleanup()
	}
}

func (app *App) handleRoomCleanup() {
	app.roomConnMu.Lock()
	defer app.roomConnMu.Unlock()
	
	if app.currentRoom == "" {
		return
	}
	
	// Decrement connection count
	if count, exists := app.roomConnections[app.currentRoom]; exists && count > 0 {
		app.roomConnections[app.currentRoom]--
		newCount := app.roomConnections[app.currentRoom]
		
		log.Infow("Connection count updated", "roomName", app.currentRoom, "connections", newCount)
		
		// If no more connections, delete the room
		if newCount == 0 {
			go app.deleteRoom(app.currentRoom)
			delete(app.roomConnections, app.currentRoom)
		}
	}
}

func (app *App) deleteRoom(roomName string) {
	if app.roomClient == nil {
		log.Errorw("Room client not initialized", fmt.Errorf("cannot delete room without room client"))
		return
	}
	
	log.Infow("Deleting empty room", "roomName", roomName)
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	_, err := app.roomClient.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
		Room: roomName,
	})
	
	if err != nil {
		log.Errorw("Failed to delete room", err, "roomName", roomName)
	} else {
		log.Infow("Room deleted successfully", "roomName", roomName)
		
		// Disconnect from LiveKit if this was our current room
		app.livekitMu.Lock()
		if app.currentRoom == roomName {
			if app.room != nil {
				app.room.Disconnect()
			}
			app.livekitReady = false
			app.currentRoom = ""
		}
		app.livekitMu.Unlock()
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

func (app *App) dispatchAgent(roomName string) {
	log.Infow("Dispatching okchat-voice-agent to room", "roomName", roomName)
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// Extract chatbot ID from room name (remove "esp-" prefix)
	chatbotId := roomName[4:]
	
	// Create agent dispatch request
	req := &livekit.CreateAgentDispatchRequest{
		Room:      roomName,
		AgentName: "okchat-voice-agent",
		Metadata:  fmt.Sprintf("{\"room_name\": \"%s\", \"chatbot_id\": \"%s\"}", roomName, chatbotId),
	}
	
	log.Infow("Agent dispatch request created", "roomName", roomName, "agentName", "okchat-voice-agent", "chatbotId", chatbotId)
	
	// Dispatch agent using the client
	dispatch, err := app.dispatchClient.CreateDispatch(ctx, req)
	if err != nil {
		log.Errorw("Failed to dispatch agent", err, "roomName", roomName, "agentName", "okchat-voice-agent")
	} else {
		log.Infow("Agent dispatched successfully", "roomName", roomName, "agentName", "okchat-voice-agent", "dispatch", dispatch)
	}
}


