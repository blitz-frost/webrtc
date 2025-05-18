package webrtc

import (
	stdbytes "bytes"
	"sync"

	"github.com/blitz-frost/bytes"
	"github.com/blitz-frost/errors"
	"github.com/blitz-frost/io/msg"
	"github.com/pion/webrtc/v3"
)

const conflictMessage = "conflict" // error message returned when a negotiation conflict is encountered

// Candidate clarifies ICECandidate transmission between peers.
type Candidate string

func CandidateOf(candidate webrtc.ICECandidate) Candidate {
	return Candidate(candidate.ToJSON().Candidate)
}

func (x Candidate) Convert() webrtc.ICECandidateInit {
	// this just reverses the webrtc.ICECandidate.ToJSON method
	// no idea why some members are set up as pointers to zero values, while others are nil
	// either way, no sense transmitting defaults
	return webrtc.ICECandidateInit{
		Candidate:     string(x),
		SDPMid:        new(string),
		SDPMLineIndex: new(uint16),
	}
}

type Channel struct {
	V *webrtc.DataChannel

	buf stdbytes.Buffer

	errorHandle func(error)
}

// ChannelMake wraps a [webrtc.DataChannel] to fit the msg framework.
func ChannelMake(v *webrtc.DataChannel) *Channel {
	x := Channel{
		V: v,
	}
	return &x
}

func (x *Channel) Close() error {
	return x.V.Close() // unclear if this triggers OnClose
}

func (x *Channel) CloseHandle(fn func()) {
	x.V.OnClose(fn)
}

// Registers an InputTaker to receive incoming messages.
// Errors it returns are passed to the error handler, if any.
func (x *Channel) InputChain(dst msg.InputTaker) {
	x.V.OnMessage(func(m webrtc.DataChannelMessage) {
		r := (*bytes.Reader)(&m.Data)
		if err := dst(msg.Input{
			Read: r.Read,
			Close: func() error {
				return nil
			},
			WriteTo: r.WriteTo,
		}); err != nil {
			if x.errorHandle != nil {
				x.errorHandle(err)
			}
		}
	})
}

func (x *Channel) OpenHandle(fn func()) {
	x.V.OnOpen(fn)
}

// Not concurrent safe.
func (x *Channel) Output() (msg.Output, error) {
	return msg.Output{
		Write: x.buf.Write,
		Close: func() error {
			b := x.buf.Bytes()
			err := x.V.Send(b)
			x.buf.Reset()
			return err
		},
		Cancel: func() error {
			x.buf.Reset()
			return nil
		},
		ReadFrom: func(r msg.Reader) (int64, error) {
			return x.buf.ReadFrom(r)
		},
	}, nil
}

// SessionDescription clarifies what is supposed to be transmitted between peers as-is.
// The pion variant is extremely unintuitive.
type SessionDescription struct {
	Type   webrtc.SDPType
	String string
}

func SessionDescriptionOf(sd webrtc.SessionDescription) SessionDescription {
	return SessionDescription{
		Type:   sd.Type,
		String: sd.SDP,
	}
}

func (x SessionDescription) Convert() webrtc.SessionDescription {
	return webrtc.SessionDescription{
		Type: x.Type,
		SDP:  x.String,
	}
}

// A Signaler handles negotiation between two remote peers.
type Signaler struct {
	conn *webrtc.PeerConnection
	set  SignalerSet

	sdpAlter func(*webrtc.SessionDescription)

	priority     bool
	priorityChan chan struct{}

	candidates      []Candidate    // the peer connection accumulates gathered candidates here
	candidateChan   chan Candidate // sending goroutine will wait for this when out of accumulated candidates
	candidateWant   bool           // true if the sending goroutine is currently waiting for more candidates
	candidateAnswer chan error     // signaled by the goroutine responsible for sending as part of an answer

	state signalerState

	waitPending []chan error // goroutines waiting to do their own negotiation
	waitSimple  []chan error // goroutines just waiting for current negotiation to end

	mux sync.Mutex
}

// SignalerMake creates a new Signaler for the given local peer connection. Each side needs a unique Signaler that is wired to communicate with the other one.
//
// peerSet - communication with the remote peer; typically set up through some form of RPC that is equivalent to calling the remote peer's Signaler methods with the same names
//
// priority - determines which peer will abandon their offer in case of concurrent conflict; peers must take opposing priorities
func SignalerMake(conn *webrtc.PeerConnection, peerSet SignalerSet, priority bool) *Signaler {
	x := &Signaler{
		conn:          conn,
		set:           peerSet,
		priority:      priority,
		priorityChan:  make(chan struct{}),
		candidateChan: make(chan Candidate),
	}

	// supposedly this starts as soon as the local description is set
	conn.OnICECandidate(x.candidateHandle)

	return x
}

// Answer responds to a remote offer.
// This method is not for direct use, but to be wired to the remote Signaler.
func (x *Signaler) Answer(offer SessionDescription) (SessionDescription, error) {
	x.mux.Lock()

	if x.state == signalerStateOffering {
		x.mux.Unlock()

		if x.priority {
			// this side sticks to offering
			return SessionDescription{}, errors.Simple(conflictMessage)
		}

		// this side will switch to answering once offering will get rejected
		<-x.priorityChan
	}

	// only one answer call can come at a time, so the only other possible state is idle

	x.state = signalerStateAnswering

	x.mux.Unlock()

	if err := x.conn.SetRemoteDescription(offer.Convert()); err != nil {
		return SessionDescription{}, err
	}

	answer, err := x.conn.CreateAnswer(nil)
	if err != nil {
		return SessionDescription{}, err
	}

	if x.sdpAlter != nil {
		x.sdpAlter(&answer)
	}

	if err = x.conn.SetLocalDescription(answer); err != nil {
		return SessionDescription{}, err
	}

	return SessionDescriptionOf(answer), nil
}

// CandidateAdd adds remote ICE candidates to the local peer connection.
// This method is not for direct use, but to be wired to the remote Signaler.
func (x *Signaler) CandidateAdd(c ...Candidate) error {
	if len(c) == 0 {
		// this is received when negotiation has ultimately failed on the offering side
		// discard local candidates and return to idle
		go func() {
			x.candidateProcessDiscard()

			x.finishAnswer(errors.Simple("peer rejected answer"))
		}()
		return nil
	}

	if x.candidateAnswer == nil {
		// start sending candidates in own goroutine
		x.candidateAnswer = make(chan error, 1)
		go func() {
			done, err := x.candidateProcessSend()

			if !done {
				x.candidateProcessDiscard()
			}

			x.candidateAnswer <- err
		}()
	}

	for _, candidate := range c {
		if candidate == "" {
			// end of remote candidates
			return x.candidateAdd_finish()
		}
		if err := x.conn.AddICECandidate(candidate.Convert()); err != nil {
			if err2 := x.candidateAdd_finish(); err2 != nil {
				e := errors.Message("add ice candidate", err)
				e.Link(err2)
				err = e
			}
			return err
		}
	}

	return nil
}

func (x *Signaler) candidateAdd_finish() error {
	// wait for all local candidates and return to idle
	err := <-x.candidateAnswer
	x.candidateAnswer = nil

	x.finishAnswer(err)
	return err
}

// Negotiate performs a full negotiation with the remote peer.
// Blocks until the whole process is finished, including ICE candidate exchange.
//
// Concurrent safe. Calls made during an active negotiation will first wait for it to finish. Multiple goroutines that are waiting at the same time will return at the same time, being treated as a batch.
//
// If both peers attempt to negotiate at the same time, the one without priority will cancel their offer, and instead wait for answering to complete before returning.
func (x *Signaler) Negotiate() error {
	var sameBatch []chan error

	x.mux.Lock()

	if x.state != signalerStateIdle {
		// wait for current negotiation to finish

		ch := make(chan error)
		x.waitPending = append(x.waitPending, ch)

		x.mux.Unlock()

		err := <-ch

		x.mux.Lock()

		if len(x.waitPending) == 0 || x.waitPending[0] != ch {
			// this goroutine ended up being part of a batch that was already handled
			x.mux.Unlock()
			return err
		}

		// this is the first goroutine of a pending batch
		// it must do the work and signal all the others
		//
		// the received error should be nil

		sameBatch = append(sameBatch, x.waitPending[1:]...)
		x.waitPending = x.waitPending[:0]
	}

	x.state = signalerStateOffering

	x.mux.Unlock()

	var (
		priorityConceded bool
		err              error
	)
	defer func() {
		if !priorityConceded { // otherwise this is left to the answer function
			x.finish()
		}

		// transmit any encountered error to goroutines in the same batch
		for _, ch := range sameBatch {
			ch <- err
		}
	}()

	offer, err := x.conn.CreateOffer(nil)
	if err != nil {
		return err
	}

	if x.sdpAlter != nil {
		x.sdpAlter(&offer)
	}

	answer, err := x.set.Answer(SessionDescriptionOf(offer))
	if err != nil {
		if err.Error() != conflictMessage {
			return err
		}

		// the peer is also sending an offer and has priority
		// just wait for that to resolve as an answer

		priorityConceded = true
		ch := make(chan error)

		x.mux.Lock()

		x.waitSimple = append(x.waitSimple, ch)

		// anything pending can be included in this batch
		sameBatch = append(sameBatch, x.waitPending...)
		x.waitPending = x.waitPending[:0]

		// unblock the answer goroutine, but keep the mutex locked
		x.priorityChan <- struct{}{}

		err = <-ch
		return err
	}

	if err = x.conn.SetLocalDescription(offer); err != nil {
		return err
	}

	// ICE candidates will now start gathering
	// arrange for them to be discarded on error, in order to finish with a clean slate
	var candidatesDone bool
	defer func() {
		if !candidatesDone {
			x.candidateProcessDiscard()
		}
	}()

	if err = x.conn.SetRemoteDescription(answer.Convert()); err != nil {
		// peer is waiting for candidates to conclude their own side
		if err2 := x.set.CandidateAdd(); err2 != nil {
			e := errors.Message("negotiation set remote answer", err)
			e.Link(err2)
			err = e
		}
		return err
	}

	// send candidates
	candidatesDone, err = x.candidateProcessSend()
	return err
}

// SdpAlter registers a function that may alter the local session description before it gets set to the local peer, or sent to the remote peer.
// Must not be called concurrently with negotiation.
func (x *Signaler) SdpAlter(fn func(*webrtc.SessionDescription)) {
	x.sdpAlter = fn
}

func (x *Signaler) candidateHandle(candidate *webrtc.ICECandidate) {
	var c Candidate
	if candidate == nil {
		// this should mark ICE gathering end

		// NOTE supposedly, delivering this to the peer is deprecated and no longer needed
		// regardless, it's unclear how exactly Pion would handle this with the ICECandidateInit type
		// so we just use an empty string for our layer, and deliver nothing to Pion

		c = ""
	} else {
		c = CandidateOf(*candidate)
	}

	x.mux.Lock()

	if x.candidateWant {
		x.candidateChan <- c
		x.candidateWant = false
	} else {
		x.candidates = append(x.candidates, c)
	}

	x.mux.Unlock()
}

func (x *Signaler) candidateProcess(fn func(...Candidate) error) (bool, error) {
	var done bool
	for c := []Candidate{}; !done; c = c[:0] {
		x.mux.Lock()

		if len(x.candidates) > 0 {
			// steal what we can, and allow the peer connection to keep accumulating
			c = append(c, x.candidates...)
			x.candidates = x.candidates[:0]
		} else {
			x.candidateWant = true
		}

		x.mux.Unlock()

		if len(c) == 0 {
			// there was nothing available, so wait for something
			c = append(c, <-x.candidateChan)
		}

		if c[len(c)-1] == "" {
			done = true
		}

		if err := fn(c...); err != nil {
			return done, err
		}
	}

	return done, nil
}

func (x *Signaler) candidateProcessDiscard() {
	x.candidateProcess(func(...Candidate) error {
		return nil
	})
}

func (x *Signaler) candidateProcessSend() (bool, error) {
	return x.candidateProcess(x.set.CandidateAdd)
}

func (x *Signaler) finish() {
	x.mux.Lock()

	if len(x.waitPending) > 0 {
		// there is a pending batch; let the first waiting goroutine handle it
		// since it's a different negotiation, the current error might not concern it
		x.waitPending[0] <- nil
	} else {
		x.state = signalerStateIdle
	}

	x.mux.Unlock()
}

func (x *Signaler) finishAnswer(err error) {
	for _, ch := range x.waitSimple { // can't be altered before returning to idle
		ch <- err
	}

	x.finish()
}

type SignalerSet struct {
	Answer       func(SessionDescription) (SessionDescription, error)
	CandidateAdd func(...Candidate) error
}

const (
	signalerStateIdle signalerState = iota
	signalerStateOffering
	signalerStateAnswering
)

type signalerState int
