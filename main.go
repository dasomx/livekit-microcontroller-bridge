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
	"encoding/json"
	"strings"

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

// ICE candidate structure for JSON parsing
type ICECandidate struct {
	Type      string `json:"type"`
	Candidate string `json:"candidate"`
	SdpMid    string `json:"sdpMid"`
	SdpMLineIndex int `json:"sdpMLineIndex"`
}

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

	log.Infow("Application started successfully", 
		"port", "3000", 
		"livekit_host", host,
		"note", "LiveKit will connect when first microcontroller connects",
		"improvements", "TURN server enabled, Trickle ICE support, Extended timeouts")

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
	log.Infow("=== LIVEKIT INITIALIZATION START ===", "roomName", roomName)
	
	app.livekitMu.Lock()
	defer app.livekitMu.Unlock()
	
	// Initialize clients if not already done
	if app.roomClient == nil {
		log.Infow("Initializing LiveKit clients", "host", host)
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
		log.Infow("=== LIVEKIT INITIALIZATION COMPLETED (REUSED) ===", "roomName", roomName)
		return nil
	}
	
	// If we're connected to a different room, disconnect first
	if app.livekitReady && app.currentRoom != roomName {
		log.Infow("Switching rooms, disconnecting from current room", "currentRoom", app.currentRoom, "newRoom", roomName)
		if app.room != nil {
			app.room.Disconnect()
			log.Infow("Disconnected from previous room", "previousRoom", app.currentRoom)
		}
		app.livekitReady = false
	}
	
	log.Infow("Creating new LiveKit connection", "roomName", roomName)
	
	var err error
	
	// Create LiveKit track
	log.Infow("Creating LiveKit RTP track", "roomName", roomName)
	livekitTrack, err = webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "pion",
	)
	if err != nil {
		log.Errorw("Failed to create LiveKit track", err, "roomName", roomName)
		return fmt.Errorf("failed to create LiveKit track: %w", err)
	}
	log.Infow("LiveKit RTP track created successfully", "roomName", roomName)

	// Generate access token with dynamic room name
	log.Infow("Generating access token", "roomName", roomName, "identity", identity)
	token, err := newAccessToken(apiKey, apiSecret, roomName, identity)
	if err != nil {
		log.Errorw("Failed to create access token", err, "roomName", roomName)
		return fmt.Errorf("failed to create access token: %w", err)
	}
	log.Infow("Access token generated successfully", "roomName", roomName)

	// Create room with callbacks
	log.Infow("Creating LiveKit room with callbacks", "roomName", roomName)
	app.room = lksdk.NewRoom(&lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: app.onTrackSubscribed,
		},
	})
	log.Infow("LiveKit room created successfully", "roomName", roomName)

	// Prepare and join room
	log.Infow("Preparing room connection", "roomName", roomName, "host", host)
	if err := app.room.PrepareConnection(host, token); err != nil {
		log.Errorw("Failed to prepare room connection", err, "roomName", roomName)
		return fmt.Errorf("failed to prepare room connection: %w", err)
	}
	log.Infow("Room connection prepared successfully", "roomName", roomName)

	log.Infow("Joining room with token", "roomName", roomName)
	if err := app.room.JoinWithToken(host, token); err != nil {
		log.Errorw("Failed to join room", err, "roomName", roomName)
		return fmt.Errorf("failed to join room: %w", err)
	}
	log.Infow("Successfully joined room", "roomName", roomName)

	// Create embedded track
	log.Infow("Creating embedded track", "roomName", roomName)
	embeddedTrack, err = lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus})
	if err != nil {
		log.Errorw("Failed to create embedded track", err, "roomName", roomName)
		return fmt.Errorf("failed to create embedded track: %w", err)
	}
	log.Infow("Embedded track created successfully", "roomName", roomName)

	// Publish track
	log.Infow("Publishing embedded track to room", "roomName", roomName)
	if _, err = app.room.LocalParticipant.PublishTrack(embeddedTrack, &lksdk.TrackPublicationOptions{
		Name: "embedded",
	}); err != nil {
		log.Errorw("Failed to publish track", err, "roomName", roomName)
		return fmt.Errorf("failed to publish track: %w", err)
	}
	log.Infow("Embedded track published successfully", "roomName", roomName)

	app.livekitReady = true
	app.currentRoom = roomName
	
	// Initialize connection count for this room
	app.roomConnMu.Lock()
	app.roomConnections[roomName] = 1
	app.roomConnMu.Unlock()
	
	log.Infow("LiveKit connection established successfully", "roomName", roomName, "connections", 1)
	
	// Dispatch the okchat-voice-agent to the room
	log.Infow("Dispatching agent to room", "roomName", roomName)
	go app.dispatchAgent(roomName)
	
	log.Infow("=== LIVEKIT INITIALIZATION COMPLETED SUCCESSFULLY ===", "roomName", roomName)
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
	log.Infow("=== HTTP SERVER STARTUP ===")
	
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Infow("Root path accessed", "path", r.URL.Path, "remoteAddr", r.RemoteAddr)
		if r.URL.Path != "/" {
			log.Infow("Path not found", "path", r.URL.Path, "remoteAddr", r.RemoteAddr)
			http.NotFound(w, r)
			return
		}
		log.Infow("Redirecting to test.html", "remoteAddr", r.RemoteAddr)
		http.Redirect(w, r, "/test.html", http.StatusFound)
	})
	mux.HandleFunc("/test.html", func(w http.ResponseWriter, r *http.Request) {
		log.Infow("Serving test.html", "remoteAddr", r.RemoteAddr)
		http.ServeFile(w, r, "test.html")
	})
	mux.HandleFunc("/connect", app.connectHandler)
	mux.HandleFunc("/ice", app.connectHandler)  // Same handler for ICE candidates

	app.server = &http.Server{
		Addr:    ":3000",
		Handler: mux,
	}
	
	log.Infow("HTTP server configured", "addr", ":3000")
	log.Infow("Starting HTTP server on :3000")
	return app.server.ListenAndServe()
}

func (app *App) connectHandler(w http.ResponseWriter, r *http.Request) {
	// Log incoming connection attempt
	log.Infow("=== CONNECTION ATTEMPT START ===", 
		"method", r.Method, 
		"path", r.URL.Path, 
		"remoteAddr", r.RemoteAddr,
		"userAgent", r.UserAgent(),
		"contentType", r.Header.Get("Content-Type"))
	
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	
	// Handle preflight OPTIONS request
	if r.Method == http.MethodOptions {
		log.Infow("Handling OPTIONS preflight request")
		w.WriteHeader(http.StatusOK)
		return
	}
	
	if r.Method != http.MethodPost {
		log.Errorw("Invalid method for connection",
			fmt.Errorf("invalid method"),     // error 값 전달
			"method", r.Method, "expected", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get chatbotId from query parameters
	chatbotId := r.URL.Query().Get("chatbotId")
	log.Infow("Query parameters received", "chatbotId", chatbotId, "allParams", r.URL.Query())
	
	if chatbotId == "" {
		log.Errorw("Missing chatbotId query parameter", fmt.Errorf("chatbotId parameter is required"))
		http.Error(w, "Missing required query parameter: chatbotId", http.StatusBadRequest)
		return
	}

	// Create room name in format "esp-{chatbotId}"
	dynamicRoomName := fmt.Sprintf("esp-%s", chatbotId)
	log.Infow("Processing connection request", "chatbotId", chatbotId, "roomName", dynamicRoomName)

	// Check content type to handle both SDP and ICE candidates
	contentType := r.Header.Get("Content-Type")
	log.Infow("Processing request", "contentType", contentType, "chatbotId", chatbotId)

	// Read request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		log.Errorw("Failed to read request body", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	log.Infow("Request body read successfully", "bodySize", len(bodyBytes), "chatbotId", chatbotId)

	// Handle ICE candidate requests (JSON)
	if strings.Contains(contentType, "application/json") {
		log.Infow("Processing ICE candidate request", "chatbotId", chatbotId)
		app.handleICECandidate(w, r, chatbotId, bodyBytes)
		return
	}

	// Handle SDP offer requests (application/sdp)
	if !strings.Contains(contentType, "application/sdp") {
		log.Errorw("Invalid content type", fmt.Errorf("expected application/sdp or application/json"), "contentType", contentType)
		http.Error(w, "Invalid content type", http.StatusBadRequest)
		return
	}

	// Initialize LiveKit connection if not already done
	log.Infow("Attempting to initialize LiveKit connection", "roomName", dynamicRoomName)
	if err := app.initializeLiveKitForRoom(dynamicRoomName); err != nil {
		log.Errorw("Failed to initialize LiveKit", err, "roomName", dynamicRoomName)
		http.Error(w, "Failed to initialize LiveKit connection", http.StatusInternalServerError)
		return
	}
	log.Infow("LiveKit connection initialized successfully", "roomName", dynamicRoomName)

	// Process SDP offer
	app.handleSDPOffer(w, r, chatbotId, dynamicRoomName, bodyBytes)
}

func (app *App) handleICECandidate(w http.ResponseWriter, r *http.Request, chatbotId string, bodyBytes []byte) {
	log.Infow("=== ICE CANDIDATE HANDLING START ===", "chatbotId", chatbotId)
	
	// Find the peer connection for this chatbot
	var targetPC *webrtc.PeerConnection
	app.peerConnMu.RLock()
	for connID, pc := range app.peerConns {
		if strings.Contains(connID, chatbotId) {
			targetPC = pc
			log.Infow("Found matching peer connection", "connID", connID, "chatbotId", chatbotId)
			break
		}
	}
	app.peerConnMu.RUnlock()

	if targetPC == nil {
		log.Errorw("No peer connection found for chatbot", fmt.Errorf("peer connection not found"), "chatbotId", chatbotId)
		http.Error(w, "No peer connection found", http.StatusNotFound)
		return
	}

	// Parse ICE candidate from JSON
	var candidate ICECandidate
	if err := json.Unmarshal(bodyBytes, &candidate); err != nil {
		log.Errorw("Failed to parse ICE candidate JSON", err, "chatbotId", chatbotId)
		http.Error(w, "Invalid ICE candidate JSON", http.StatusBadRequest)
		return
	}

	log.Infow("Parsed ICE candidate", "candidate", candidate.Candidate, "chatbotId", chatbotId)

	// Add ICE candidate to peer connection
	sdpMLineIndex := uint16(candidate.SdpMLineIndex)
	if err := targetPC.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: candidate.Candidate,
		SDPMid:    &candidate.SdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	}); err != nil {
		log.Errorw("Failed to add ICE candidate", err, "chatbotId", chatbotId)
		http.Error(w, "Failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	log.Infow("ICE candidate added successfully", "chatbotId", chatbotId)
	
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ICE candidate added"))
	
	log.Infow("=== ICE CANDIDATE HANDLING COMPLETED ===", "chatbotId", chatbotId)
}

func (app *App) handleSDPOffer(w http.ResponseWriter, r *http.Request, chatbotId, dynamicRoomName string, offer []byte) {
	log.Infow("=== SDP OFFER HANDLING START ===", "chatbotId", chatbotId)

	log.Infow("Creating WebRTC peer connection", "chatbotId", chatbotId)
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{
					"stun:stun.l.google.com:19302",
					"stun:stun1.l.google.com:19302",
					"stun:stun2.l.google.com:19302",
				},
			},
			// Add TURN server for NAT traversal
			{
				URLs: []string{
					"turn:relay1.expressturn.com:3478",
				},
				Username:   "ef3CQZKPX1CGQP3K9P",
				Credential: "Bvce9qkPVJgNJnmR",
			},
		},
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
		ICECandidatePoolSize: 10,
	})
	if err != nil {
		log.Errorw("Failed to create peer connection", err, "chatbotId", chatbotId)
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}
	log.Infow("WebRTC peer connection created successfully", "chatbotId", chatbotId)

	// Store peer connection for cleanup with chatbot ID for easier lookup
	connID := fmt.Sprintf("%s-%p", chatbotId, pc)
	app.peerConnMu.Lock()
	app.peerConns[connID] = pc
	app.peerConnMu.Unlock()
	log.Infow("Peer connection stored for cleanup", "connID", connID, "chatbotId", chatbotId)

	// Setup track handler
	log.Infow("Setting up track handler for peer connection", "chatbotId", chatbotId)
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Infow("Track received from peer connection", "trackKind", track.Kind(), "chatbotId", chatbotId)
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
	log.Infow("Adding LiveKit track to peer connection", "chatbotId", chatbotId)
	if _, err = pc.AddTrack(livekitTrack); err != nil {
		log.Errorw("Failed to add track to peer connection", err, "chatbotId", chatbotId)
		http.Error(w, "Failed to add track", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	}
	log.Infow("LiveKit track added to peer connection successfully", "chatbotId", chatbotId)

	// Log local ICE candidates as they are gathered (useful for debugging)
	log.Infow("Setting up ICE candidate handler", "chatbotId", chatbotId)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			candidateJSON := c.ToJSON()
			log.Infow("Local ICE candidate gathered", 
				"candidate", candidateJSON.Candidate, 
				"chatbotId", chatbotId)
		} else {
			log.Infow("ICE gathering complete (server)", "chatbotId", chatbotId)
		}
	})

	// ---------------------------------------------------------------------------------
	// Register handler to accept ESP32-created DataChannel
	// Server does NOT create a DataChannel; it only listens.
	// ---------------------------------------------------------------------------------
	log.Infow("Setting up DataChannel handler", "chatbotId", chatbotId)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Infow("ESP32 DataChannel opened", "label", dc.Label(), "id", dc.ID(), "chatbotId", chatbotId)
		dc.OnOpen(func() {
			log.Infow("DataChannel ready", "label", dc.Label(), "chatbotId", chatbotId)
			// Send handshake message to start AI conversation
			if err := dc.SendText(`{"type":"request.create"}`); err != nil {
				log.Errorw("Failed to send handshake message", err, "chatbotId", chatbotId)
			} else {
				log.Infow("Handshake request.create sent", "chatbotId", chatbotId)
			}
			// Send session configuration and initial response after delays
			go func() {
				time.Sleep(400 * time.Millisecond)
				sessionCfg := `{"type":"session.update","session":{"modalities":["text","audio"],"instructions":"You are a helpful assistant. Please greet the user and ask how you can help.","voice":"alloy","input_audio_format":"pcm16","output_audio_format":"pcm16"}}`
				if err := dc.SendText(sessionCfg); err != nil {
					log.Errorw("Failed to send session.update", err, "chatbotId", chatbotId)
				} else {
					log.Infow("session.update sent", "chatbotId", chatbotId)
				}
				time.Sleep(400 * time.Millisecond)
				respCreate := `{"type":"response.create","response":{"modalities":["text","audio"],"instructions":"Hello! How can I help you today?"}}`
				if err := dc.SendText(respCreate); err != nil {
					log.Errorw("Failed to send response.create", err, "chatbotId", chatbotId)
				} else {
					log.Infow("response.create sent", "chatbotId", chatbotId)
				}
			}()
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString {
				log.Infow("DataChannel text received", "message", string(msg.Data), "chatbotId", chatbotId)
				// No echo; AI will handle the response
			} else {
				log.Infow("DataChannel binary received", "size", len(msg.Data), "chatbotId", chatbotId)
			}
		})
		dc.OnClose(func() {
			log.Infow("DataChannel closed", "label", dc.Label(), "chatbotId", chatbotId)
		})
		dc.OnError(func(err error) {
			log.Errorw("DataChannel error", err, "label", dc.Label(), "chatbotId", chatbotId)
		})
	})

	// Setup ICE connection state change handler
	log.Infow("Setting up ICE connection state change handler", "chatbotId", chatbotId)
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Infow("ICE connection state changed", "state", state, "chatbotId", chatbotId)
		
		switch state {
		case webrtc.ICEConnectionStateConnected:
			log.Infow("ICE connection established successfully", "chatbotId", chatbotId)
		case webrtc.ICEConnectionStateCompleted:
			log.Infow("ICE connection completed", "chatbotId", chatbotId)
		case webrtc.ICEConnectionStateChecking:
			log.Infow("ICE connection checking candidates", "chatbotId", chatbotId)
		case webrtc.ICEConnectionStateFailed:
			log.Errorw("ICE connection failed - will retry", fmt.Errorf("ICE connection failed"), "chatbotId", chatbotId)
			// Don't cleanup immediately, give it time to recover
			go func() {
				time.Sleep(10 * time.Second)
				if pc.ICEConnectionState() == webrtc.ICEConnectionStateFailed {
					log.Infow("ICE connection still failed after retry period, cleaning up", "chatbotId", chatbotId)
					app.cleanupPeerConnection(connID)
				}
			}()
		case webrtc.ICEConnectionStateDisconnected:
			log.Infow("ICE connection disconnected - monitoring for recovery", "chatbotId", chatbotId)
			// Give some time for reconnection
			go func() {
				time.Sleep(30 * time.Second)
				if pc.ICEConnectionState() == webrtc.ICEConnectionStateDisconnected {
					log.Infow("ICE connection still disconnected after grace period, cleaning up", "chatbotId", chatbotId)
					app.cleanupPeerConnection(connID)
				}
			}()
		case webrtc.ICEConnectionStateClosed:
			log.Infow("ICE connection closed, cleaning up", "chatbotId", chatbotId)
			app.cleanupPeerConnection(connID)
		}
	})

	// Setup connection state change handler for additional debugging
	log.Infow("Setting up connection state change handler", "chatbotId", chatbotId)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Infow("Peer connection state changed", "state", state, "chatbotId", chatbotId)
	})

	// Setup signaling state change handler
	log.Infow("Setting up signaling state change handler", "chatbotId", chatbotId)
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		log.Infow("Signaling state changed", "state", state, "chatbotId", chatbotId)
	})

	// Set remote description
	log.Infow("Setting remote description (offer)", "chatbotId", chatbotId)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, 
		SDP:  string(offer),
	}); err != nil {
		log.Errorw("Failed to set remote description", err, "chatbotId", chatbotId)
		http.Error(w, "Failed to set remote description", http.StatusBadRequest)
		app.cleanupPeerConnection(connID)
		return
	}
	log.Infow("Remote description set successfully", "chatbotId", chatbotId)

	// Create answer
	log.Infow("Creating answer SDP", "chatbotId", chatbotId)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Errorw("Failed to create answer", err, "chatbotId", chatbotId)
		http.Error(w, "Failed to create answer", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	}
	log.Infow("Answer SDP created successfully", "chatbotId", chatbotId)

	// Set local description
	log.Infow("Setting local description (answer)", "chatbotId", chatbotId)
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Errorw("Failed to set local description", err, "chatbotId", chatbotId)
		http.Error(w, "Failed to set local description", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	}
	log.Infow("Local description set successfully", "chatbotId", chatbotId)

	// Wait for ICE gathering to complete with extended timeout
	log.Infow("Waiting for ICE gathering to complete", "chatbotId", chatbotId)
	select {
	case <-webrtc.GatheringCompletePromise(pc):
		log.Infow("ICE gathering completed successfully", "chatbotId", chatbotId)
	case <-time.After(30 * time.Second):  // Extended timeout for TURN servers
		log.Errorw("ICE gathering timeout",
			fmt.Errorf("ice gathering timeout"),  // error 값 전달
			"chatbotId", chatbotId)
		http.Error(w, "ICE gathering timeout", http.StatusInternalServerError)
		app.cleanupPeerConnection(connID)
		return
	case <-app.ctx.Done():
		log.Infow("Context cancelled during ICE gathering", "chatbotId", chatbotId)
		http.Error(w, "Server shutting down", http.StatusServiceUnavailable)
		app.cleanupPeerConnection(connID)
		return
	}

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusOK)
	
	log.Infow("Sending SDP answer to client", "chatbotId", chatbotId, "sdpLength", len(pc.LocalDescription().SDP))
	if _, err := fmt.Fprint(w, pc.LocalDescription().SDP); err != nil {
		log.Errorw("Failed to write response", err, "chatbotId", chatbotId)
	}
	
	log.Infow("=== CONNECTION ATTEMPT COMPLETED SUCCESSFULLY ===", "chatbotId", chatbotId, "roomName", dynamicRoomName)
}

func (app *App) cleanupPeerConnection(connID string) {
	log.Infow("=== PEER CONNECTION CLEANUP START ===", "connID", connID)
	
	app.peerConnMu.Lock()
	defer app.peerConnMu.Unlock()
	
	if pc, exists := app.peerConns[connID]; exists {
		log.Infow("Closing peer connection", "connID", connID)
		if err := pc.Close(); err != nil {
			log.Errorw("Failed to close peer connection", err, "connID", connID)
		} else {
			log.Infow("Peer connection closed successfully", "connID", connID)
		}
		delete(app.peerConns, connID)
		log.Infow("Peer connection removed from map", "connID", connID)
		
		// Handle room cleanup when connection is closed
		log.Infow("Triggering room cleanup", "connID", connID)
		go app.handleRoomCleanup()
	} else {
		log.Infow("Peer connection not found in map", "connID", connID)
	}
	
	log.Infow("=== PEER CONNECTION CLEANUP COMPLETED ===", "connID", connID)
}

func (app *App) handleRoomCleanup() {
	log.Infow("=== ROOM CLEANUP START ===")
	
	app.roomConnMu.Lock()
	defer app.roomConnMu.Unlock()
	
	if app.currentRoom == "" {
		log.Infow("No current room to clean up")
		return
	}
	
	log.Infow("Checking room connection count", "roomName", app.currentRoom)
	
	// Decrement connection count
	if count, exists := app.roomConnections[app.currentRoom]; exists && count > 0 {
		app.roomConnections[app.currentRoom]--
		newCount := app.roomConnections[app.currentRoom]
		
		log.Infow("Connection count updated", "roomName", app.currentRoom, "connections", newCount)
		
		// If no more connections, schedule room deletion with extended grace period
		if newCount == 0 {
			log.Infow("No more connections, scheduling room deletion after extended grace period", "roomName", app.currentRoom)
			roomToDelete := app.currentRoom
			go func() {
				time.Sleep(120 * time.Second) // Extended grace period for reconnection attempts
				app.roomConnMu.Lock()
				defer app.roomConnMu.Unlock()
				if cnt, ok := app.roomConnections[roomToDelete]; !ok || cnt == 0 {
					log.Infow("Grace period elapsed, deleting room", "roomName", roomToDelete)
					app.deleteRoom(roomToDelete)
					delete(app.roomConnections, roomToDelete)
				} else {
					log.Infow("New connections appeared, abort room deletion", "roomName", roomToDelete, "connections", cnt)
				}
			}()
		} else {
			log.Infow("Room still has active connections", "roomName", app.currentRoom, "connections", newCount)
		}
	} else {
		log.Infow("Room connection count not found or already zero", "roomName", app.currentRoom)
	}
	
	log.Infow("=== ROOM CLEANUP COMPLETED ===")
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


