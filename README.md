# LiveKit Microcontroller Bridge

# Table of Contents

- [Why](#why)
- [Running](#running)
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

```
go run . -host=$URL_TO_LIVEKIT -api-key=$API_KEY -api-secret=$API_SECRET -room-name=$ROOM_NAME -identity=$NAME_FOR_DEVICE_IN_ROOM
```

Next you need to connect your embedded device to your running server. The easiest way is to take openai_demo and modify the URL
[here](https://github.com/espressif/esp-webrtc-solution/blob/2ac873762bc64f1cd56f18454ff58e2cd641b92c/solutions/openai_demo/main/openai_signaling.c#L22]).
I am running this in my LAN, but for me I changed that URL to http://192.168.1.93:8080/connect

When the devices comes on it will connect to that, and you will have flowing bi-directional audio.

## TODO

Everything! This repo is very basic, if people find it useful I will improve it.
