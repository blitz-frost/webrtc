package webrtc

import (
	"sync"

	"github.com/blitz-frost/io"
	"github.com/blitz-frost/io/msg"
	"github.com/blitz-frost/rpc"
	"github.com/pion/webrtc/v4"
)

var (
	CandidateProcedureName = "webrtcCandidate"
	SdpProcedureName       = "webrtcSdp"
)

type Channel struct {
	V *webrtc.DataChannel

	buf []byte // buffer outgoing messages
	dst msg.ReaderTaker
}

// ChannelNew wraps a [webrtc.DataChannel] to fit the msg framework.
func ChannelNew(v *webrtc.DataChannel) *Channel {
	x := Channel{
		V:   v,
		buf: []byte{},
		dst: msg.Void{},
	}
	v.OnMessage(func(m webrtc.DataChannelMessage) {
		x.dst.ReaderTake((*io.BytesReader)(&m.Data))
	})
	return &x
}

func (x *Channel) Close() error {
	return x.V.Close()
}

func (x *Channel) CloseHandle(fn func()) {
	x.V.OnClose(fn)
}

/*
Not present in wasm version
func (x Channel) ErrorHandle(fn func(error)) {
	x.ch.OnError(fn)
}
*/

func (x *Channel) OpenHandle(fn func()) {
	x.V.OnOpen(fn)
}

func (x *Channel) ReaderChain(dst msg.ReaderTaker) error {
	x.dst = dst
	return nil
}

// The returned value is also a [msg.Canceler].
// Not concurrent safe.
func (x *Channel) Writer() (msg.Writer, error) {
	return (*writer)(x), nil
}

// Sdp separates the webrtc.SessionDescription exported part, making it encoding agnostic.
type Sdp struct {
	Type   webrtc.SDPType
	String string
}

type signaler struct {
	fnCandidate func(string) error
	fnSdp       func(Sdp) error
}

func (x *signaler) candidate(candidate *webrtc.ICECandidate) error {
	return x.fnCandidate(candidate.ToJSON().Candidate)
}

func (x *signaler) sdp(sd webrtc.SessionDescription) error {
	return x.fnSdp(Sdp{
		Type:   sd.Type,
		String: sd.SDP,
	})
}

func (x *signaler) setup(conn *webrtc.PeerConnection, cli rpc.Client, lib rpc.Library, answerFunc func() error) error {
	pending := make([]*webrtc.ICECandidate, 0)
	mux := sync.Mutex{}

	// answer side
	lib.Register(CandidateProcedureName, func(s string) error {
		zero := uint16(0)
		empty := ""
		ci := webrtc.ICECandidateInit{
			Candidate:     s,
			SDPMid:        &empty,
			SDPMLineIndex: &zero,
		}
		return conn.AddICECandidate(ci)
	})
	lib.Register(SdpProcedureName, func(s Sdp) error {
		sdp := webrtc.SessionDescription{
			Type: s.Type,
			SDP:  s.String,
		}
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

	// call side
	cli.Bind(CandidateProcedureName, &x.fnCandidate)
	cli.Bind(SdpProcedureName, &x.fnSdp)

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

// SignalAnswer sets up the WebRTC answer side of the signaling process for a peer connection.
//
// The underlying RPC system must be capable of concurrent, as well as recursive calls.
// Two procedures will be added, whose names are determined by the global variables CandidateProcedureName and SdpProcedureName.
func SignalAnswer(conn *webrtc.PeerConnection, cli rpc.Client, lib rpc.Library) error {
	sig := signaler{}
	answerFunc := func() error {
		answer, err := conn.CreateAnswer(nil)
		if err != nil {
			return err
		}

		go sig.sdp(answer)

		return conn.SetLocalDescription(answer)
	}

	return sig.setup(conn, cli, lib, answerFunc)
}

// SignalOffer sets up the WebRTC offer side of the signaling process for a peer connection.
//
// The underlying RPC system must be capable of concurrent, as well as recursive calls.
// Two procedures will be added, whose names are determined by the global variables CandidateProcedureName and SdpProcedureName.
//
// The returned function can be used to start the initial process, as well as renegotiation.
func SignalOffer(conn *webrtc.PeerConnection, cli rpc.Client, lib rpc.Library) (func() error, error) {
	sig := signaler{}
	answerFunc := func() error { return nil }

	if err := sig.setup(conn, cli, lib, answerFunc); err != nil {
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

		return sig.sdp(offer)
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
