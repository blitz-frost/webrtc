//go:build !(wasm && js)

package webrtc

func (x *Channel) ErrorHandle(fn func(error)) {
	x.errorHandle = fn
	x.V.OnError(fn)
}
