package webrtc

import (
	"sync"

	"github.com/blitz-frost/encoding/json"
	msgenc "github.com/blitz-frost/encoding/msg"
	"github.com/blitz-frost/io"
	msgio "github.com/blitz-frost/io/msg"
	"github.com/blitz-frost/msg"
	"github.com/blitz-frost/rpc"
	"github.com/pion/webrtc/v3"
)

type Channel struct {
	V *webrtc.DataChannel

	buf []byte // buffer outgoing messages
	dst msgio.ReaderTaker
}

// NewChannel wraps a [webrtc.DataChannel] to fit the msg framework.
func NewChannel(v *webrtc.DataChannel) *Channel {
	x := Channel{
		V:   v,
		buf: []byte{},
		dst: msgio.Void{},
	}
	v.OnMessage(func(m webrtc.DataChannelMessage) {
		x.dst.ReaderTake((*io.BytesReader)(&m.Data))
	})
	return &x
}

func (x *Channel) Close() error {
	return x.V.Close()
}

func (x *Channel) OnClose(fn func()) {
	x.V.OnClose(fn)
}

/*
Not present in wasm version
func (x Channel) OnError(fn func(error)) {
	x.ch.OnError(fn)
}
*/

func (x *Channel) OnOpen(fn func()) {
	x.V.OnOpen(fn)
}

func (x *Channel) ReaderChain(dst msgio.ReaderTaker) error {
	x.dst = dst
	return nil
}

// The returned value is also a [msg.Canceler].
// Not concurrent safe.
func (x *Channel) Writer() (msgio.Writer, error) {
	return (*writer)(x), nil
}

type signaler struct {
	fnCandidate func(webrtc.ICECandidateInit) error
	fnSdp       func(webrtc.SessionDescription) error
}

func (x *signaler) candidate(candidate *webrtc.ICECandidate) error {
	arg := candidate.ToJSON()
	return x.fnCandidate(arg)
}

func (x *signaler) setup(conn *webrtc.PeerConnection, c msgio.Conn, rCh, wCh byte, answerFunc func() error) error {
	// ensure concurrency
	rc, err := msgio.ReaderChainerAsyncNew(c)
	if err != nil {
		return err
	}
	wg := msgio.WriterGiverMutexNew(c)
	block := msg.ConnBlock[msgio.Reader, msgio.Writer]{rc, wg}

	// form ExchangeConn
	mc, err := msgio.MultiplexConnOf(block)
	if err != nil {
		return err
	}
	rConn := msgio.ConnOf(mc, rCh)
	wConn := msgio.ConnOf(mc, wCh)

	ec, err := msgio.ExchangeConnOf(rConn, wConn)
	if err != nil {
		return err
	}
	ecEnc, err := msgenc.ExchangeConnOf(ec, json.Codec)
	if err != nil {
		return err
	}

	pending := make([]*webrtc.ICECandidate, 0)
	mux := sync.Mutex{}

	// prepare rpc answer side
	lib := rpc.MakeLibrary()
	lib.Register("candidate", func(c webrtc.ICECandidateInit) error {
		return conn.AddICECandidate(c)
	})
	lib.Register("sdp", func(sdp webrtc.SessionDescription) error {
		if err := conn.SetRemoteDescription(sdp); err != nil {
			return err
		}

		if err := answerFunc(); err != nil {
			return err
		}

		mux.Lock()
		for _, candidate := range pending {
			go x.candidate(candidate)
		}
		mux.Unlock()

		return nil
	})

	answerGate := rpc.MakeAnswerGate(lib)
	if err := ecEnc.ReaderChain(answerGate); err != nil {
		return err
	}

	// prepare rpc call side
	callGate := rpc.MakeCallGate(ecEnc)
	cli := rpc.MakeClient(callGate)

	cli.Bind("candidate", &x.fnCandidate)
	cli.Bind("sdp", &x.fnSdp)

	conn.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		mux.Lock()
		defer mux.Unlock()

		desc := conn.RemoteDescription()
		if desc == nil {
			pending = append(pending, candidate)
		} else {
			go x.candidate(candidate)
		}
	})

	return nil
}

// SignalAnswer sets up the WebRTC answer side of the signaling process for a peer connection, using the provided data carrier.
func SignalAnswer(conn *webrtc.PeerConnection, c msgio.Conn) error {
	sig := signaler{}
	answerFunc := func() error {
		answer, err := conn.CreateAnswer(nil)
		if err != nil {
			return err
		}

		go sig.fnSdp(answer)

		return conn.SetLocalDescription(answer)
	}

	return sig.setup(conn, c, 1, 0, answerFunc)
}

// SignalOffer sets up the WebRTC offer side of the signaling process for a peer connection, using the provided data carrier.
// The returned function can be used to start the initial process, as well as renegotiation.
func SignalOffer(conn *webrtc.PeerConnection, c msgio.Conn) (func() error, error) {
	sig := signaler{}
	answerFunc := func() error { return nil }

	if err := sig.setup(conn, c, 0, 1, answerFunc); err != nil {
		return nil, err
	}

	fn := func() error {
		offer, err := conn.CreateOffer(nil)
		if err != nil {
			return err
		}
		if err = conn.SetLocalDescription(offer); err != nil {
			return err
		}

		return sig.fnSdp(offer)
	}

	return fn, nil
}

type writer Channel

func (x *writer) Cancel() error {
	x.buf = x.buf[:0]
	return nil
}

func (x *writer) Close() error {
	err := x.V.Send(x.buf)
	x.buf = x.buf[:0]
	return err
}

func (x *writer) Write(b []byte) (int, error) {
	x.buf = append(x.buf, b...)
	return len(b), nil
}
