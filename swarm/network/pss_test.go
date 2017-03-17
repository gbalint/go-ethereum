package network

import (
	"testing"

	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/p2p/adapters"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	p2ptest "github.com/ethereum/go-ethereum/p2p/testing"
)

type pssTester struct {
	*p2ptest.ProtocolTester
}

func TestPssTwoToSelf(t *testing.T) {
	addr := RandomAddr()
	pt := newPssTester(t, addr, 2)
	hs_pivot := correctBzzHandshake(addr)
	for _, id := range pt.Ids {
		hs_sim := correctBzzHandshake(NewPeerAddrFromNodeId(id))
		glog.V(logger.Detail).Infof("Will handshake %v with %v", hs_pivot, hs_sim)
		<-pt.GetPeer(id).Connc
		pt.TestExchanges(bzzHandshakeExchange(hs_pivot, hs_sim, id)...)
	}
}

func newPssTester(t *testing.T, addr *peerAddr, n int) *pssTester {
	return newPssBaseTester(t, addr, n)
}

func newPssBaseTester(t *testing.T, addr *peerAddr, n int) *pssTester {
	ct := BzzCodeMap()
	ct.Register(&PssMsg{})

	simPipe := adapters.NewSimPipe
	kp := NewKadParams()
	to := NewKademlia(addr.OverlayAddr(), kp)
	pp := NewHive(NewHiveParams(), to)
	net := simulations.NewNetwork(&simulations.NetworkConfig{})
	naf := func(conf *simulations.NodeConfig) adapters.NodeAdapter {
		na := adapters.NewSimNode(conf.Id, net, simPipe)
		return na
	}
	net.SetNaf(naf)

	srv := func(p Peer) error {
		p.Register(&PssMsg{}, PssMsgHandler)
		pp.Add(p)
		p.DisconnectHook(func(err error) {
			pp.Remove(p)
		})
		return nil
	}
	protocall := func(na adapters.NodeAdapter) adapters.ProtoCall {
		protocol := Bzz(addr.OverlayAddr(), na, ct, srv, nil, nil)
		return protocol.Run
	}

	s := p2ptest.NewProtocolTester(t, NodeId(addr), n, protocall)

	return &pssTester{
		ProtocolTester: s,
	}

}