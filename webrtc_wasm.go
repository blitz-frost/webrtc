//go:build wasm && js

// Package webrtc wraps the Javascript WebRTC API.
// Currently mostly just complements the pion/webrtc package.
package webrtc

import (
	"syscall/js"

	"github.com/blitz-frost/wasm"
	"github.com/blitz-frost/wasm/media"
)

type Conn struct {
	v js.Value

	trackHandler js.Func
}

func ConnOf(v js.Value) *Conn {
	return &Conn{
		v: v,
	}
}

func (x Conn) TrackAdd(track media.Track) (Sender, error) {
	v, err := wasm.Call(x.v, "addTrack", track.Js())
	return Sender{v}, err
}

func (x *Conn) TrackHandle(fn func(track media.Track, streams []media.Stream)) {
	x.trackHandler.Release()

	x.trackHandler = js.FuncOf(func(this js.Value, args []js.Value) any {
		track := media.AsTrack(args[0].Get("track"))
		streamsJs := args[0].Get("streams")
		var streams []media.Stream
		for i, n := 0, streamsJs.Length(); i < n; i++ {
			v := streamsJs.Index(i)
			streams = append(streams, media.AsStream(v))
		}

		fn(track, streams)
		return nil
	})

	x.v.Set("ontrack", x.trackHandler)
}

func (x Conn) Release() {
	x.trackHandler.Release()
}

// All properties are defined as optional in the JS API, so they may return zero values.
type CodecParameters struct {
	v js.Value
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
	v js.Value
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
	v js.Value
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
	v js.Value
}

func (x Sender) Parameters() SendParameters {
	v := x.v.Call("getParameters")
	return SendParameters{v}
}

// Must be called with the return value of the last Parameters method call.
func (x Sender) ParametersSet(params SendParameters) error {
	promise := x.v.Call("setParameters", params.v)
	_, err := wasm.Await(promise)
	return err
}

func (x Sender) TrackReplace(track media.Track) error {
	promise := x.v.Call("replaceTrack", track.Js())
	_, err := wasm.Await(promise)
	return err
}
