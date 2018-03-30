package peers

import (
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/stretchr/testify/suite"

	"github.com/status-im/status-go/geth/params"
)

type PeerPoolSimulationSuite struct {
	suite.Suite

	bootnode *p2p.Server
	peers    []*p2p.Server
}

func TestPeerPoolSimulationSuite(t *testing.T) {
	suite.Run(t, new(PeerPoolSimulationSuite))
}

func (s *PeerPoolSimulationSuite) freePort() int {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	s.Require().NoError(err)
	l, err := net.ListenTCP("tcp", addr)
	s.Require().NoError(err)
	return l.Addr().(*net.TCPAddr).Port
}

func (s *PeerPoolSimulationSuite) SetupTest() {
	// :0 can't be passed to p2p config, cause it will be used for Self()
	// which breaks things
	port := s.freePort()
	key, _ := crypto.GenerateKey()
	name := common.MakeName("bootnode", "1.0")
	// 127.0.0.1 is invalidated by discovery v5
	s.bootnode = &p2p.Server{
		Config: p2p.Config{
			MaxPeers:    10,
			Name:        name,
			ListenAddr:  fmt.Sprintf("0.0.0.0:%d", port),
			PrivateKey:  key,
			DiscoveryV5: true,
			NoDiscovery: true,
		},
	}
	s.Require().NoError(s.bootnode.Start())
	bootnodeV5 := discv5.NewNode(s.bootnode.DiscV5.Self().ID, net.ParseIP("127.0.0.1"), uint16(port), uint16(port))

	s.peers = make([]*p2p.Server, 2)
	for i := range s.peers {
		key, _ := crypto.GenerateKey()
		peer := &p2p.Server{
			Config: p2p.Config{
				MaxPeers:         10,
				Name:             common.MakeName("peer-"+strconv.Itoa(i), "1.0"),
				ListenAddr:       fmt.Sprintf("0.0.0.0:%d", s.freePort()),
				PrivateKey:       key,
				DiscoveryV5:      true,
				NoDiscovery:      true,
				BootstrapNodesV5: []*discv5.Node{bootnodeV5},
			},
		}
		s.NoError(peer.Start())
		s.peers[i] = peer
	}
}

func (s *PeerPoolSimulationSuite) TestSingleTopicDiscovery() {
	topic := discv5.Topic("cap=test")
	expectedConnections := 1
	// simulation should only rely on fast sync
	config := map[discv5.Topic]params.Limits{
		topic: {expectedConnections, expectedConnections},
	}
	peerPool := NewPeerPool(config, 100*time.Millisecond, 100*time.Millisecond, nil)
	register := NewRegister(topic)
	s.Require().NoError(register.Start(s.peers[0]))
	defer register.Stop()
	// need to wait for topic to get registered, discv5 can query same node
	// for a topic only once a minute
	events := make(chan *p2p.PeerEvent, 20)
	subscription := s.peers[1].SubscribeEvents(events)
	defer subscription.Unsubscribe()
	s.NoError(peerPool.Start(s.peers[1]))
	defer peerPool.Stop()
	connected := 0
	for {
		select {
		case ev := <-events:
			if ev.Type == p2p.PeerEventTypeAdd {
				connected++
			}
		case <-time.After(5 * time.Second):
			s.Require().FailNowf("waiting for peers timed out", strconv.Itoa(connected))
		}
		if connected == expectedConnections {
			break
		}
	}
}

func (s *PeerPoolSimulationSuite) TearDown() {
	s.bootnode.Stop()
	for _, p := range s.peers {
		p.Stop()
	}
}

type PeerPoolIsolatedSuite struct {
	suite.Suite

	peer     *p2p.Server
	peerPool *PeerPool
	topic    discv5.Topic
}

func TestPeerPoolIsolatedSuite(t *testing.T) {
	suite.Run(t, new(PeerPoolIsolatedSuite))
}

func (s *PeerPoolIsolatedSuite) SetupTest() {
	key, _ := crypto.GenerateKey()
	name := common.MakeName("peer", "1.0")
	s.peer = &p2p.Server{
		Config: p2p.Config{
			MaxPeers:    10,
			Name:        name,
			ListenAddr:  "0.0.0.0:0",
			PrivateKey:  key,
			NoDiscovery: true,
		},
	}
	s.Require().NoError(s.peer.Start())
	s.topic = discv5.Topic("cap=cap1")
	config := map[discv5.Topic]params.Limits{
		s.topic: {1, 2},
	}
	s.peerPool = NewPeerPool(config, 100*time.Millisecond, 300*time.Millisecond, nil)
	s.peerPool.init()
	s.peerPool.quit = make(chan struct{})
}

func (s *PeerPoolIsolatedSuite) TearDown() {
	s.peer.Stop()
	close(s.peerPool.quit)
}

func (s *PeerPoolIsolatedSuite) AssertConsumed(channel chan time.Duration, expected time.Duration, timeout time.Duration) {
	select {
	case received := <-channel:
		s.Equal(expected, received)
	case <-time.After(timeout):
		s.FailNow("timed out waiting")
	}
}

func (s *PeerPoolIsolatedSuite) TestSyncSwitches() {
	period := make(chan time.Duration, 2)
	found := make(chan *discv5.Node, 1)
	lookup := make(chan bool, 1)
	events := make(chan *p2p.PeerEvent, 1)
	go s.peerPool.handlePeersFromTopic(s.peer, s.topic, period, found, lookup, events)
	s.AssertConsumed(period, s.peerPool.fastSync, time.Second)
	testPeer := discv5.NewNode(discv5.NodeID{1}, s.peer.Self().IP, 32311, 32311)
	found <- testPeer
	events <- &p2p.PeerEvent{
		Type: p2p.PeerEventTypeAdd,
		Peer: discover.NodeID(testPeer.ID),
	}
	s.AssertConsumed(period, s.peerPool.slowSync, time.Second)
	s.True(s.peerPool.peers[s.topic][testPeer.ID].connected)
	events <- &p2p.PeerEvent{
		Type:  p2p.PeerEventTypeDrop,
		Peer:  discover.NodeID(testPeer.ID),
		Error: p2p.DiscProtocolError.Error(),
	}
	s.AssertConsumed(period, s.peerPool.fastSync, time.Second)
}

func (s *PeerPoolIsolatedSuite) TestNewPeerSelectedOnDrop() {
	peer1 := discv5.NewNode(discv5.NodeID{1}, s.peer.Self().IP, 32311, 32311)
	peer2 := discv5.NewNode(discv5.NodeID{2}, s.peer.Self().IP, 32311, 32311)
	peer3 := discv5.NewNode(discv5.NodeID{3}, s.peer.Self().IP, 32311, 32311)
	// add 3 nodes and confirm connection for 1 and 2
	s.peerPool.processFoundNode(s.peer, 0, s.topic, peer1)
	s.peerPool.processFoundNode(s.peer, 1, s.topic, peer2)
	s.peerPool.processFoundNode(s.peer, 2, s.topic, peer3)
	s.True(s.peerPool.processAddedNode(s.peer, 0, discover.NodeID(peer1.ID), s.topic))
	s.True(s.peerPool.processAddedNode(s.peer, 1, discover.NodeID(peer2.ID), s.topic))
	s.False(s.peerPool.processAddedNode(s.peer, 2, discover.NodeID(peer3.ID), s.topic))
	s.False(s.peerPool.peers[s.topic][peer3.ID].connected)

	s.True(s.peerPool.processDroppedNode(s.peer, s.topic, discover.NodeID(peer1.ID), p2p.DiscNetworkError.Error()))
}

func (s *PeerPoolIsolatedSuite) TestRequestedDoesntRemove() {
	peer1 := discv5.NewNode(discv5.NodeID{1}, s.peer.Self().IP, 32311, 32311)
	s.peerPool.processFoundNode(s.peer, 0, s.topic, peer1)
	s.True(s.peerPool.processAddedNode(s.peer, 0, discover.NodeID(peer1.ID), s.topic))
	s.False(s.peerPool.processDroppedNode(s.peer, s.topic, discover.NodeID(peer1.ID), p2p.DiscRequested.Error()))
	s.Contains(s.peerPool.peers[s.topic], peer1.ID)
	s.True(s.peerPool.processDroppedNode(s.peer, s.topic, discover.NodeID(peer1.ID), p2p.DiscProtocolError.Error()))
	s.NotContains(s.peerPool.peers[s.topic], peer1.ID)
}