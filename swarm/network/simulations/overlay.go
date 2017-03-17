// +build none

// You can run this simulation using
//
//    go run ./swarm/network/simulations/overlay.go
package main

import (
	"math/rand"
	"runtime"
	"time"

	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/adapters"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	p2ptest "github.com/ethereum/go-ethereum/p2p/testing"
	"github.com/ethereum/go-ethereum/swarm/network"
)

// Network extends simulations.Network with hives for each node.
type Network struct {
	*simulations.Network
	hives []*network.Hive
}

// SimNode is the adapter used by Swarm simulations.
type SimNode struct {
	connect func(s string) error
	hive    *network.Hive
	adapters.NodeAdapter
}

// the hive update ticker for hive
func af() <-chan time.Time {
	return time.NewTicker(1 * time.Second).C
}

// Start() starts up the hive
// makes SimNode implement *NodeAdapter
func (self *SimNode) Start() error {
	return self.hive.Start(self.connect, af)
}

// Stop() shuts down the hive
// makes SimNode implement *NodeAdapter
func (self *SimNode) Stop() error {
	self.hive.Stop()
	return nil
}

func (self *SimNode) RunProtocol(id *adapters.NodeId, rw, rrw p2p.MsgReadWriter, peer *adapters.Peer) error {
	return self.NodeAdapter.(adapters.ProtocolRunner).RunProtocol(id, rw, rrw, peer)
}

// NewSimNode creates adapters for nodes in the simulation.
func (self *Network) NewSimNode(conf *simulations.NodeConfig) adapters.NodeAdapter {
	id := conf.Id
	na := adapters.NewSimNode(id, self.Network, adapters.NewSimPipe)
	addr := network.NewPeerAddrFromNodeId(id)
	kp := network.NewKadParams()
	kp.MinProxBinSize = 4
	kp.MinBinSize = 4
	to := network.NewKademlia(addr.OverlayAddr(), kp) // overlay topology driver
	// to := network.NewTestOverlay(addr.OverlayAddr())   // overlay topology driver
	hp := network.NewHiveParams()
	hp.CallInterval = 5000
	pp := network.NewHive(hp, to) // hive
	self.hives = append(self.hives, pp)                // remember hive
	// bzz protocol Run function. messaging through SimPipe
	
	services := func(p network.Peer) error {
		dp := network.NewDiscovery(p, to)
		pp.Add(dp)
		glog.V(logger.Detail).Infof("kademlia on %v", p)
		p.DisconnectHook(func(err error) {
			pp.Remove(dp)
		})
		return nil
	}
	
	ct := network.BzzCodeMap(network.HiveMsgs...) // bzz protocol code map
	na.Run = network.Bzz(addr.OverlayAddr(), na, ct, services, nil, nil).Run
	connect := func(s string) error {
		return self.Connect(id, adapters.NewNodeIdFromHex(s))
	}
	return &SimNode{
		connect:     connect,
		hive:        pp,
		NodeAdapter: na,
	}
}

func NewNetwork(network *simulations.Network) *Network {
	n := &Network{
		// hives:
		Network: network,
	}
	n.SetNaf(n.NewSimNode)
	return n
}

func nethook(conf *simulations.NetworkConfig) (simulations.NetworkControl, *simulations.ResourceController) {
	conf.DefaultMockerConfig = simulations.DefaultMockerConfig()
	conf.DefaultMockerConfig.SwitchonRate = 100
	// conf.DefaultMockerConfig.NodesTarget = 15
	conf.DefaultMockerConfig.NewConnCount = 1
	conf.DefaultMockerConfig.DegreeTarget = 0
	conf.Id = "0"
	conf.Backend = true
	net := NewNetwork(simulations.NewNetwork(conf))

	ids := p2ptest.RandomNodeIds(10)

	for i, id := range ids {
		net.NewNode(&simulations.NodeConfig{Id: id})
		var peerId *adapters.NodeId
		if i == 0 {
			peerId = ids[len(ids)-1]
		} else {
			peerId = ids[i-1]
		}
		err := net.hives[i].Register(network.NewPeerAddrFromNodeId(peerId))
		if err != nil {
			panic(err.Error())
		}
	}
	go func() {
		for _, id := range ids {
			net.NewNode(&simulations.NodeConfig{Id: id})
			net.Start(id)
			glog.V(logger.Debug).Infof("node %v starting up", id)
			n := rand.Intn(1000)
			time.Sleep(time.Duration(n) * time.Millisecond)
			// net.Stop(id)
		}
	}()
	// go func() {
	// for _, id := range ids {
	// 	net.Stop(id)
	// 	glog.V(logger.Debug).Infof("node %v shutting down", id)
	// 	n := rand.Intn(500)
	// 	time.Sleep(time.Duration(n) * time.Millisecond)
	// }
	// }()
	return net, nil
}

// var server
func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	glog.SetV(logger.Detail)
	glog.SetToStderr(true)

	c, quitc := simulations.NewSessionController(nethook)

	simulations.StartRestApiServer("8888", c)
	// wait until server shuts down
	<-quitc

}