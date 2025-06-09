# LiveKit Microcontroller Bridge

# Table of Contents

- [Why](#why)
- [Running](#running)
- [Docker](#docker)
- [TODO](#TODO)

## Why?

[Video](https://youtu.be/wCxmrXFUyTs)

Hardware/software has reached a point where a single hacker or small team can build something amazing
with microcontrollers and media. You can purchase a dev board and build something yourself. You don't
need to be a specialist (or pay one) for hardware or media specifics.

Building [this](https://www.youtube.com/watch?v=0z7QJxZWFQg) got me very excited about what is possible.

[LiveKit](https://livekit.io/) is a great server for connecting Mobile, Web and other clients together via WebRTC.
Espressif has [esp-webrtc-solution](https://github.com/espressif/esp-webrtc-solution), but it doesn't connect to LiveKit.
The requirements for LiveKit clients are too heavy for a microcontroller (two PeerConnections and a Websocket).
So this repo acts as bridge so you can connect a microcontroller into LiveKit.

## Running

### Native Go

```
go run . -host=$URL_TO_LIVEKIT -api-key=$API_KEY -api-secret=$API_SECRET -room-name=$ROOM_NAME -identity=$NAME_FOR_DEVICE_IN_ROOM
```

Next you need to connect your embedded device to your running server. The easiest way is to take openai_demo and modify the URL
[here](https://github.com/espressif/esp-webrtc-solution/blob/2ac873762bc64f1cd56f18454ff58e2cd641b92c/solutions/openai_demo/main/openai_signaling.c#L22]).
I am running this in my LAN, but for me I changed that URL to http://192.168.1.93:8080/connect

When the devices comes on it will connect to that, and you will have flowing bi-directional audio.

## Docker

### Using Docker Compose (Recommended)

1. Copy the example environment file and configure your LiveKit settings:

   ```bash
   cp env.example .env
   ```

2. Edit `.env` with your LiveKit configuration:

   ```bash
   LIVEKIT_HOST=wss://your-livekit-server.com
   LIVEKIT_API_KEY=your-api-key
   LIVEKIT_API_SECRET=your-api-secret
   ROOM_NAME=embedded
   IDENTITY=microcontroller-bridge
   ```

3. Build and run with docker-compose:
   ```bash
   docker-compose up --build
   ```

### Using Docker directly

1. Build the Docker image:

   ```bash
   docker build -t livekit-microcontroller-bridge .
   ```

2. Run the container:
   ```bash
   docker run -p 8080:8080 \
     -e LIVEKIT_HOST=wss://your-livekit-server.com \
     -e LIVEKIT_API_KEY=your-api-key \
     -e LIVEKIT_API_SECRET=your-api-secret \
     livekit-microcontroller-bridge \
     -host=wss://your-livekit-server.com \
     -api-key=your-api-key \
     -api-secret=your-api-secret \
     -room-name=embedded \
     -identity=microcontroller-bridge
   ```

### Connecting Your Microcontroller

After the bridge is running (either natively or in Docker), connect your embedded device to:

- `http://YOUR_HOST_IP:8080/connect` (replace `YOUR_HOST_IP` with your actual IP address)

For example, if running locally: `http://192.168.1.93:8080/connect`

## TODO

Everything! This repo is very basic, if people find it useful I will improve it.
