package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

var (
	host, apiKey, apiSecret, roomName, identity string
	livekitTrack                                *webrtc.TrackLocalStaticRTP
	embeddedTrack                               *lksdk.LocalTrack
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "embedded", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "debug"}, "livekit-embedded-bridge")
	lksdk.SetLogger(logger.GetLogger())
	flag.Parse()
	if host == "" || apiKey == "" || apiSecret == "" || roomName == "" || identity == "" {
		fmt.Println("invalid arguments.")
		return
	}

	var err error
	livekitTrack, err = webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "pion",
	)
	if err != nil {
		panic(err)
	}

	token, err := newAccessToken(apiKey, apiSecret, roomName, identity)
	if err != nil {
		panic(err)
	}

	room := lksdk.NewRoom(&lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				for {
					rtpPacket, _, rtpErr := track.ReadRTP()
					if rtpErr != nil {
						return
					}

					if rtpErr = livekitTrack.WriteRTP(rtpPacket); rtpErr != nil {
						return
					}
				}
			},
		},
	})

	if err := room.PrepareConnection(host, token); err != nil {
		panic(err)
	}

	if err := room.JoinWithToken(host, token); err != nil {
		panic(err)
	}

	embeddedTrack, err = lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus})
	if err != nil {
		panic(err)
	}

	if _, err = room.LocalParticipant.PublishTrack(embeddedTrack, &lksdk.TrackPublicationOptions{
		Name: "embedded",
	}); err != nil {
		panic(err)
	}

	http.HandleFunc("/connect", connectHandler)
	log.Println("Server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func connectHandler(w http.ResponseWriter, r *http.Request) {
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			for {
				rtpPacket, _, rtpErr := track.ReadRTP()
				if rtpErr != nil {
					return
				}

				if rtpErr = embeddedTrack.WriteRTP(rtpPacket, nil); rtpErr != nil {
					return
				}
			}
		}
	})

	if _, err = pc.AddTrack(livekitTrack); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Println("ICE state:", state)
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-webrtc.GatheringCompletePromise(pc)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	fmt.Fprint(w, pc.LocalDescription().SDP) //nolint: errcheck
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
