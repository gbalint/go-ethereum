// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package network

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/protocols"
	bv "github.com/ethereum/go-ethereum/swarm/network/bitvector"
	pq "github.com/ethereum/go-ethereum/swarm/network/priorityqueue"
	"github.com/ethereum/go-ethereum/swarm/storage"
)

const (
	HashSize = 32

	Low uint8 = iota
	Mid
	High
	Top
	PriorityQueue        // number of queues
	PriorityQueueCap = 3 // queue capacity
)

// Handover represents a statement that the upstream peer hands over the stream section
type Handover struct {
	Stream     string // name of stream
	Start, End uint64 // index of hashes
	Root       []byte // Root hash for indexed segment inclusion proofs
}

// HandoverProof represents a signed statement that the upstream peer handed over the stream section
type HandoverProof struct {
	Sig []byte // Sign(Hash(Serialisation(Handover)))
	*Handover
}

// Takeover represents a statement that downstream peer took over (stored all data)
// handed over
type Takeover Handover

//  TakeoverProof represents a signed statement that the downstream peer took over
// the stream section
type TakeoverProof struct {
	Sig []byte // Sign(Hash(Serialisation(Takeover)))
	*Takeover
}

// TakeoverProofMsg is the protocol msg sent by downstream peer
type TakeoverProofMsg TakeoverProof

// String pretty prints TakeoverProofMsg
func (self TakeoverProofMsg) String() string {
	return fmt.Sprintf("Stream: '%v' [%v-%v], Root: %x, Sig: %x", self.Stream, self.Start, self.End, self.Root, self.Sig)
}

// SubcribeMsg is the protocol msg for requesting a stream(section)
type SubscribeMsg struct {
	Stream   string
	Key      []byte
	From, To uint64
	Priority uint8 // delivered on priority channel
}

// UnsyncedKeysMsg is the protocol msg for offering to hand over a
// stream section
type UnsyncedKeysMsg struct {
	Stream         string // name of Stream
	Key            []byte // subtype or key
	From, To       uint64 // peer and db-specific entry count
	Hashes         []byte // stream of hashes (128)
	*HandoverProof        // HandoverProof
}

/*
 store requests are put in netstore so they are stored and then
 forwarded to the peers in their kademlia proximity bin by the syncer
*/
type ChunkDeliveryMsg struct {
	Key   storage.Key
	SData []byte // the stored chunk Data (incl size)
	// optional
	Id   uint64 // request ID. if delivery, the ID is retrieve request ID
	from Peer   // [not serialised] protocol registers the requester
}

// String pretty prints UnsyncedKeysMsg
func (self UnsyncedKeysMsg) String() string {
	return fmt.Sprintf("Stream '%v' [%v-%v] (%v)", self.Stream, self.From, self.To, len(self.Hashes)/HashSize)
}

// WantedKeysMsg is the protocol msg data for signaling which hashes
// offered in UnsyncedKeysMsg downstream peer actually wants sent over
type WantedKeysMsg struct {
	Stream   string // name of stream
	Key      []byte // subtype or key
	Want     []byte // bitvector indicating which keys of the batch needed
	From, To uint64 // next interval offset - empty if not to be continued
}

// String pretty prints WantedKeysMsg
func (self WantedKeysMsg) String() string {
	return fmt.Sprintf("Stream '%v', Want: %x, Next: [%v-%v]", self.Stream, self.Want, self.From, self.To)
}

// Streamer registry for outgoing and incoming streamer constructors
type Streamer struct {
	incomingLock sync.RWMutex
	outgoingLock sync.RWMutex
	peersLock    sync.RWMutex
	outgoing     map[string]func(*StreamerPeer, []byte) (OutgoingStreamer, error)
	incoming     map[string]func(*StreamerPeer, []byte) (IncomingStreamer, error)

	dbAccess *DbAccess
	overlay  Overlay
	receiveC chan *ChunkDeliveryMsg
	peers    map[discover.NodeID]*StreamerPeer
}

// NewStreamer is Streamer constructor
func NewStreamer(overlay Overlay, dbAccess *DbAccess) *Streamer {
	return &Streamer{
		outgoing: make(map[string]func(*StreamerPeer, []byte) (OutgoingStreamer, error)),
		incoming: make(map[string]func(*StreamerPeer, []byte) (IncomingStreamer, error)),
		dbAccess: dbAccess,
		overlay:  overlay,
		receiveC: make(chan *ChunkDeliveryMsg, 10),
		peers:    make(map[discover.NodeID]*StreamerPeer),
	}
}

// RegisterIncomingStreamer registers an incoming streamer constructor
func (self *Streamer) RegisterIncomingStreamer(stream string, f func(*StreamerPeer, []byte) (IncomingStreamer, error)) {
	self.incomingLock.Lock()
	defer self.incomingLock.Unlock()
	self.incoming[stream] = f
}

// RegisterOutgoingStreamer registers an outgoing streamer constructor
func (self *Streamer) RegisterOutgoingStreamer(stream string, f func(*StreamerPeer, []byte) (OutgoingStreamer, error)) {
	self.outgoingLock.Lock()
	defer self.outgoingLock.Unlock()
	self.outgoing[stream] = f
}

// GetIncomingStreamer accessor for incoming streamer constructors
func (self *Streamer) GetIncomingStreamer(stream string) (func(*StreamerPeer, []byte) (IncomingStreamer, error), error) {
	self.incomingLock.RLock()
	defer self.incomingLock.RUnlock()
	f := self.incoming[stream]
	if f == nil {
		return nil, fmt.Errorf("stream %v not registered", stream)
	}
	return f, nil
}

// GetOutgoingStreamer accessor for incoming streamer constructors
func (self *Streamer) GetOutgoingStreamer(stream string) (func(*StreamerPeer, []byte) (OutgoingStreamer, error), error) {
	self.outgoingLock.RLock()
	defer self.outgoingLock.RUnlock()
	f := self.outgoing[stream]
	if f == nil {
		return nil, fmt.Errorf("stream %v not registered", stream)
	}
	return f, nil
}

func (self *Streamer) NodeInfo() interface{} {
	return nil
}

func (self *Streamer) PeerInfo(id discover.NodeID) interface{} {
	return nil
}

type outgoingStreamer struct {
	OutgoingStreamer
	priority     uint8
	currentBatch []byte
}

// OutgoingStreamer interface for outgoing peer Streamer
type OutgoingStreamer interface {
	SetNextBatch(uint64, uint64) (hashes []byte, from uint64, to uint64, proof *HandoverProof, err error)
	GetData([]byte) []byte
}

type incomingStreamer struct {
	IncomingStreamer
	priority  uint8
	intervals *Intervals
	sessionAt uint64
	live      bool
	quit      chan struct{}
	next      chan struct{}
}

// IncomingStreamer interface for incoming peer Streamer
type IncomingStreamer interface {
	NeedData([]byte) func()
	BatchDone(string, uint64, []byte, []byte) func() (*TakeoverProof, error)
}

// StreamerPeer is the Peer extention for the streaming protocol
type StreamerPeer struct {
	Peer
	streamer *Streamer
	pq       *pq.PriorityQueue
	//netStore     storage.ChunkStore
	dbAccess     *DbAccess
	outgoingLock sync.RWMutex
	incomingLock sync.RWMutex
	outgoing     map[string]*outgoingStreamer
	incoming     map[string]*incomingStreamer
	quit         chan struct{}
}

// NewStreamerPeer is the constructor for StreamerPeer
func NewStreamerPeer(p Peer, streamer *Streamer) *StreamerPeer {
	self := &StreamerPeer{
		Peer:     p,
		pq:       pq.New(int(PriorityQueue), PriorityQueueCap),
		streamer: streamer,
		outgoing: make(map[string]*outgoingStreamer),
		incoming: make(map[string]*incomingStreamer),
		quit:     make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go self.pq.Run(ctx, func(i interface{}) { p.Send(i) })
	go func() {
		<-self.quit
		cancel()
	}()
	return self
}

// RetrieveRequestMsg is the protocol msg for chunk retrieve requests
type RetrieveRequestMsg struct {
	Key storage.Key
}

func (self *StreamerPeer) handleRetrieveRequestMsg(req *RetrieveRequestMsg) error {
	chunk, created := self.dbAccess.getOrCreateRequest(req.Key)
	s, err := self.getOutgoingStreamer(retrieveRequestStream)
	if err != nil {
		return err
	}
	streamer := s.OutgoingStreamer.(*RetrieveRequestStreamer)
	if chunk.ReqC != nil {
		if created {
			if err := self.streamer.Retrieve(chunk); err != nil {
				return err
			}
		}
		go func() {
			t := time.NewTicker(3 * time.Minute)
			defer t.Stop()

			select {
			case <-chunk.ReqC:
			case <-self.quit:
				return
			case <-t.C:
				return
			}

			streamer.deliveryC <- chunk
		}()
		return nil
	}
	// TODO: call the retrieve function of the outgoing syncer
	streamer.deliveryC <- chunk
	return nil
}

// Retrieve sends a chunk retrieve request to
func (self *Streamer) Retrieve(chunk *storage.Chunk) error {
	self.overlay.EachConn(chunk.Key[:], 255, func(p OverlayConn, po int, nn bool) bool {
		sp := p.(*StreamerPeer)
		// TODO: skip light nodes that do not accept retrieve requests
		sp.SendPriority(&RetrieveRequestMsg{
			Key: chunk.Key[:],
		}, Top)
		return false
	})
	return nil
}

func (self *Streamer) getPeer(peerId discover.NodeID) *StreamerPeer {
	if self.peers == nil {
		return nil
	}
	self.peersLock.RLock()
	defer self.peersLock.RUnlock()
	return self.peers[peerId]
}

func (self *Streamer) setPeer(peer *StreamerPeer) {
	if self.peers == nil {
		self.peers = make(map[discover.NodeID]*StreamerPeer)
	}
	self.peersLock.Lock()
	self.peers[peer.ID()] = peer
	self.peersLock.Unlock()
}

func (self *Streamer) deletePeer(peer *StreamerPeer) {
	self.peersLock.Lock()
	delete(self.peers, peer.ID())
	self.peersLock.Unlock()
}

func (self *StreamerPeer) handleChunkDeliveryMsg(req *ChunkDeliveryMsg) error {
	chunk, err := self.dbAccess.get(req.Key)
	if err != nil {
		return err
	}

	self.streamer.receiveC <- req

	log.Trace(fmt.Sprintf("delivery of %v from %v", chunk, self))
	return nil
}

func (self *Streamer) processReceivedChunks() {
	for {
		select {
		case req := <-self.receiveC:
			chunk, err := self.dbAccess.get(req.Key)
			if err != nil {
				continue
			}
			chunk.SData = req.SData
			self.dbAccess.put(chunk)
			close(chunk.ReqC)
		}
	}
}

func (self *StreamerPeer) getOutgoingStreamer(s string) (*outgoingStreamer, error) {
	self.outgoingLock.RLock()
	defer self.outgoingLock.RUnlock()
	streamer := self.outgoing[s]
	if streamer == nil {
		return nil, fmt.Errorf("stream '%v' not provided", s)
	}
	return streamer, nil
}

func (self *StreamerPeer) getIncomingStreamer(s string) (*incomingStreamer, error) {
	self.incomingLock.RLock()
	defer self.incomingLock.RUnlock()
	streamer := self.incoming[s]
	if streamer == nil {
		return nil, fmt.Errorf("stream '%v' not provided", s)
	}
	return streamer, nil
}

func (self *StreamerPeer) setOutgoingStreamer(s string, o OutgoingStreamer, priority uint8) (*outgoingStreamer, error) {
	self.outgoingLock.Lock()
	defer self.outgoingLock.Unlock()
	if self.outgoing[s] != nil {
		return nil, fmt.Errorf("stream %v already registered", s)
	}
	os := &outgoingStreamer{
		OutgoingStreamer: o,
		priority:         priority,
	}
	self.outgoing[s] = os
	return os, nil
}

func (self *StreamerPeer) setIncomingStreamer(s string, i IncomingStreamer, priority uint8, live bool) error {
	self.incomingLock.Lock()
	defer self.incomingLock.Unlock()
	if self.incoming[s] != nil {
		return fmt.Errorf("stream %v already registered", s)
	}
	next := make(chan struct{}, 1)
	// var intervals *Intervals
	// if !live {
	// key := s + self.ID().String()
	// intervals = NewIntervals(key, self.streamer)
	// }
	self.incoming[s] = &incomingStreamer{
		IncomingStreamer: i,
		// intervals:        intervals,
		live:     live,
		priority: priority,
		next:     next,
	}
	next <- struct{}{} // this is to allow wantedKeysMsg before first batch arrives
	return nil
}

// NextBatch adjusts the indexes by inspecting the intervals
func (self *incomingStreamer) nextBatch(from uint64) (nextFrom uint64, nextTo uint64) {
	var intervals []uint64
	if self.live {
		if len(intervals) == 0 {
			intervals = []uint64{self.sessionAt, from}
		} else {
			intervals[1] = from
		}
		nextFrom = from
	} else if from >= self.sessionAt { // history sync complete
		intervals = nil
	} else if len(intervals) > 2 && from >= intervals[2] { // filled a gap in the intervals
		intervals = append(intervals[:1], intervals[3:]...)
		nextFrom = intervals[1]
		if len(intervals) > 2 {
			nextTo = intervals[2]
		} else {
			nextTo = self.sessionAt
		}
	} else {
		nextFrom = from
		intervals[1] = from
		nextTo = self.sessionAt
	}
	// self.intervals.set(intervals)
	return nextFrom, nextTo
}

// Subscribe initiates the streamer
func (self *Streamer) Subscribe(peerId discover.NodeID, s string, t []byte, from, to uint64, priority uint8, live bool) error {
	f, err := self.GetIncomingStreamer(s)
	if err != nil {
		return err
	}

	peer := self.getPeer(peerId)
	if peer == nil {
		return fmt.Errorf("peer not found %v", peerId)
	}

	is, err := f(peer, t)
	if err != nil {
		return err
	}
	err = peer.setIncomingStreamer(s, is, priority, live)
	if err != nil {
		return err
	}

	msg := &SubscribeMsg{
		Stream:   s,
		Key:      t,
		From:     from,
		To:       to,
		Priority: priority,
	}
	peer.SendPriority(msg, priority)
	return nil
}

func (self *StreamerPeer) handleSubscribeMsg(req *SubscribeMsg) error {
	f, err := self.streamer.GetOutgoingStreamer(req.Stream)
	if err != nil {
		return err
	}
	s, err := f(self, req.Key)
	if err != nil {
		return err
	}
	key := req.Stream + string(req.Key)
	os, err := self.setOutgoingStreamer(key, s, req.Priority)
	if err != nil {
		return nil
	}
	go self.SendUnsyncedKeys(os, req.From, req.To)
	return nil
}

// handleUnsyncedKeysMsg protocol msg handler calls the incoming streamer interface
// Filter method
func (self *StreamerPeer) handleUnsyncedKeysMsg(req *UnsyncedKeysMsg) error {
	s, err := self.getIncomingStreamer(req.Stream)
	if err != nil {
		return err
	}
	hashes := req.Hashes
	want, err := bv.New(len(hashes) / HashSize)
	if err != nil {
		return err
	}
	wg := sync.WaitGroup{}
	for i := 0; i < len(hashes)/HashSize; i += HashSize {
		hash := hashes[i : i+HashSize]
		if wait := s.NeedData(hash); wait != nil {
			want.Set(i, true)
			wg.Add(1)
			// create request and wait until the chunk data arrives and is stored
			go func(w func()) {
				w()
				wg.Done()
			}(wait)
		}
	}
	go func() {
		wg.Wait()
		if tf := s.BatchDone(req.Stream, req.From, hashes, req.Root); tf != nil {
			tp, err := tf()
			if err != nil {
				return
			}
			self.SendPriority(tp, s.priority)
		}
		s.next <- struct{}{}
	}()
	// only send wantedKeysMsg if all missing chunks of the previous batch arrived
	// except
	if s.live {
		s.sessionAt = req.From
	}
	from, to := s.nextBatch(req.To)
	if from == to {
		return nil
	}
	msg := &WantedKeysMsg{
		Stream: req.Stream,
		Want:   want.Bytes(),
		From:   from,
		To:     to,
	}
	go func() {
		select {
		case <-s.next:
		case <-s.quit:
			return
		}
		self.SendPriority(msg, s.priority)
	}()
	return nil
}

// handleWantedKeysMsg protocol msg handler
// * sends the next batch of unsynced keys
// * sends the actual data chunks as per WantedKeysMsg
func (self *StreamerPeer) handleWantedKeysMsg(req *WantedKeysMsg) error {
	s, err := self.getOutgoingStreamer(req.Stream)
	if err != nil {
		return err
	}
	hashes := s.currentBatch
	// launch in go routine since GetBatch blocks until new hashes arrive
	go self.SendUnsyncedKeys(s, req.From, req.To)
	l := len(hashes) / HashSize
	want, err := bv.NewFromBytes(req.Want, l)
	if err != nil {
		return err
	}
	for i := 0; i < l; i++ {
		if want.Get(i) {
			hash := hashes[i*HashSize : (i+1)*HashSize]
			data := s.GetData(hash)
			if data == nil {
				return errors.New("not found")
			}
			chunk := storage.NewChunk(hash, nil)
			chunk.SData = data
			if err := self.Deliver(chunk, s.priority); err != nil {
				return err
			}
		}
	}
	return nil
}

func (self *StreamerPeer) handleTakeoverProofMsg(req *TakeoverProofMsg) error {
	_, err := self.getOutgoingStreamer(req.Stream)
	if err != nil {
		return err
	}
	// store the strongest takeoverproof for the stream in streamer
	return nil
}

// Deliver sends a storeRequestMsg protocol message to the peer
func (self *StreamerPeer) Deliver(chunk *storage.Chunk, priority uint8) error {
	msg := &ChunkDeliveryMsg{
		Key:   chunk.Key,
		SData: chunk.SData,
	}
	return self.pq.Push(nil, msg, int(priority))
}

// Deliver sends a storeRequestMsg protocol message to the peer
func (self *StreamerPeer) SendPriority(msg interface{}, priority uint8) error {
	return self.pq.Push(nil, msg, int(priority))
}

// UnsyncedKeys sends UnsyncedKeysMsg protocol msg
func (self *StreamerPeer) SendUnsyncedKeys(s *outgoingStreamer, f, t uint64) error {
	hashes, from, to, proof, err := s.SetNextBatch(f, t)
	if err != nil {
		return err
	}
	s.currentBatch = hashes
	msg := &UnsyncedKeysMsg{
		HandoverProof: proof,
		Hashes:        hashes,
		From:          from,
		To:            to,
	}
	return self.SendPriority(msg, s.priority)
}

// StreamerSpec is the spec of the streamer protocol.
var StreamerSpec = &protocols.Spec{
	Name:       "stream",
	Version:    1,
	MaxMsgSize: 10 * 1024 * 1024,
	Messages: []interface{}{
		HandshakeMsg{},
		UnsyncedKeysMsg{},
		WantedKeysMsg{},
		TakeoverProofMsg{},
		SubscribeMsg{},
	},
}

// Run protocol run function
func (s *Streamer) Run(p *bzzPeer) error {
	sp := NewStreamerPeer(p, s)
	// load saved intervals
	// autosubscribe to request handler to serve request only for non-light nodes
	// sp.handleSubscribeMsg(&SubscribeMsg{
	// 	Stream:   retrieveRequeststring,
	// 	Priority: uint8(Top),
	// })
	// subscribe to request handling ; only with non-light nodes

	s.setPeer(sp)

	defer s.deletePeer(sp)

	s.Subscribe(sp.ID(), retrieveRequestStream, nil, 0, 0, Top, true)
	defer close(sp.quit)
	return sp.Run(sp.HandleMsg)
}

// HandleMsg is the message handler that delegates incoming messages
func (self *StreamerPeer) HandleMsg(msg interface{}) error {
	switch msg := msg.(type) {

	case *SubscribeMsg:
		return self.handleSubscribeMsg(msg)

	case *UnsyncedKeysMsg:
		return self.handleUnsyncedKeysMsg(msg)

	case *TakeoverProofMsg:
		return self.handleTakeoverProofMsg(msg)

	case *WantedKeysMsg:
		return self.handleWantedKeysMsg(msg)

	case *ChunkDeliveryMsg:
		return self.handleChunkDeliveryMsg(msg)

	default:
		return fmt.Errorf("unknown message type: %T", msg)
	}
}