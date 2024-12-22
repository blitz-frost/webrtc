//go:build wasm && js

// Package webrtc wraps the Javascript WebRTC API.
// Currently mostly just complements the pion/webrtc package.
package webrtc

import (
	"github.com/blitz-frost/wasm"
	"github.com/blitz-frost/wasm/media"
)

type Conn struct {
	V wasm.Object

	trackFn wasm.DynamicFunction
}

func (x *Conn) TrackAdd(track *media.Track) (Sender, error) {
	v, err := x.V.Call("addTrack", track.V)
	return Sender{v}, err
}

func (x *Conn) TrackHandle(fn func(track *media.Track, streams []media.Stream)) {
	inter := func(this wasm.Value, args []wasm.Value) (wasm.Any, error) {
		track := media.Track{}
		track.V = args[0].Get("track")
		streamsJs := args[0].Get("streams")
		var streams []media.Stream
		for i, n := 0, streamsJs.Length(); i < n; i++ {
			v := streamsJs.Index(i)
			streams = append(streams, media.Stream{v})
		}

		fn(&track, streams)
		return nil, nil
	}
	x.trackFn.Remake(wasm.InterfaceFunc(inter))

	x.V.Set("ontrack", x.trackFn.Value())
}

func (x *Conn) TransceiverAddKind(kind media.Kind) (Transceiver, error) {
	v, err := x.V.Call("addTransceiver", string(kind))
	return Transceiver{v}, err
}

func (x *Conn) TransceiverAddTrack(track *media.Track) (Transceiver, error) {
	v, err := x.V.Call("addTransceiver", track.V)
	return Transceiver{v}, err
}

func (x *Conn) Wipe() {
	x.trackFn.Wipe()
}

// All properties are defined as optional in the JS API, so they may return zero values.
type CodecParameters struct {
	v wasm.Value
}

func (x CodecParameters) Channels() uint {
	v := x.v.Get("channels")
	if v.IsUndefined() {
		return 0
	}
	return uint(v.Int())
}

func (x CodecParameters) ClockRate() uint {
	v := x.v.Get("clockRate")
	if v.IsUndefined() {
		return 0
	}
	return uint(v.Int())
}

func (x CodecParameters) MimeType() string {
	v := x.v.Get("mimeType")
	if v.IsUndefined() {
		return ""
	}
	return v.String()
}

func (x CodecParameters) PayloadType() byte {
	v := x.v.Get("payloadType")
	if v.IsUndefined() {
		return 0
	}
	return byte(v.Int())
}

func (x CodecParameters) Sdp() string {
	v := x.v.Get("sdpFmtpLine")
	if v.IsUndefined() {
		return ""
	}
	return v.String()
}

type EncodingParameters struct {
	v wasm.Value
}

func (x EncodingParameters) Active() bool {
	return x.v.Get("active").Bool()
}

func (x EncodingParameters) Downscale() float64 {
	return x.v.Get("scaleResolutionDownBy").Float()
}

// Only for video tracks.
// factor must be >= 1 and is applied to both image dimensions.
func (x EncodingParameters) DownscaleSet(factor float64) {
	x.v.Set("scaleResolutionDownBy", factor)
}

func (x EncodingParameters) BitrateMax() uint {
	v := x.v.Get("maxBitrate")
	return uint(v.Int())
}

func (x EncodingParameters) BitrateMaxSet(br uint) {
	x.v.Set("maxBitrate", br)
}

func (x EncodingParameters) FramerateMax() float64 {
	return x.v.Get("maxFramerate").Float()
}

func (x EncodingParameters) FramerateMaxSet(fps float64) {
	x.v.Set("maxFramerate", fps)
}

func (x EncodingParameters) PtimeSet(ms uint) {
	x.v.Set("ptime", ms)
}

type SendParameters struct {
	v wasm.Value
}

func (x SendParameters) Codecs() []CodecParameters {
	codecs := x.v.Get("codecs")

	n := codecs.Length()
	o := make([]CodecParameters, n)
	for i := 0; i < n; i++ {
		v := codecs.Index(i)
		o[i] = CodecParameters{v}
	}

	return o
}

// Modify the return values directly, then call Sender.ParametersSet(x).
func (x SendParameters) Encodings() []EncodingParameters {
	encodings := x.v.Get("encodings")

	n := encodings.Length()
	o := make([]EncodingParameters, n)
	for i := 0; i < n; i++ {
		v := encodings.Index(i)
		o[i] = EncodingParameters{v}
	}

	return o
}

type Sender struct {
	v wasm.Value
}

func (x Sender) Parameters() SendParameters {
	v := x.v.Call("getParameters")
	return SendParameters{v}
}

// Must be called with the return value of the last Parameters method call.
func (x Sender) ParametersSet(params SendParameters) error {
	promise := wasm.Promise(x.v.Call("setParameters", params.v))
	_, err := promise.Await()
	return err
}

func (x Sender) TrackReplace(track *media.Track) error {
	promise := wasm.Promise(x.v.Call("replaceTrack", track.V))
	_, err := promise.Await()
	return err
}

type Transceiver struct {
	v wasm.Value
}

func (x Transceiver) DirectionSet(v TransceiverDirection) {
	x.v.Set("direction", string(v))
}

type TransceiverDirection string

const (
	TransceiverDirectionBoth     TransceiverDirection = "sendrecv"
	TransceiverDirectionInactive TransceiverDirection = "inactive"
	TransceiverDirectionReceive  TransceiverDirection = "recvonly"
	TransceiverDirectionSend     TransceiverDirection = "sendonly"
	TransceiverDirectionStopped  TransceiverDirection = "stopped"
)
