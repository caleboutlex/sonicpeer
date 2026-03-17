package gossip

import (
	"time"

	"github.com/0xsoniclabs/sonic/logger"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
)

// Service is a lightweight implementation of node.Service that only handles
// P2P message routing without participating in consensus or maintaining state.
type Service struct {
	config Config // Gossip and protocol configuration settings

	p2pServer *p2p.Server // The underlying P2P server from the node stack
	Name      string

	handler             *handler       // The core protocol handler that manages peer lifecycles and messages
	operaDialCandidates enode.Iterator // DNS-based discovery iterator for finding initial peers

	// txFeed allows local RPC clients to subscribe to new transactions seen by the sentry
	txFeed event.Feed
	txCh   chan *types.Transaction

	logger.Instance
}

// NewService creates a new  gossip service.
func NewService(stack *node.Node, config Config, datadir string, url string, genesis common.Hash, networkId uint64) (*Service, error) {
	// Derive the local node ID from the P2P server's private key
	localNodeId := enode.PubkeyToIDV4(&stack.Server().PrivateKey.PublicKey)
	svc, err := newService(config, localNodeId, networkId, genesis, url, datadir)
	if err != nil {
		return nil, err
	}

	svc.Name = "Sonic"
	svc.p2pServer = stack.Server()

	return svc, nil
}

// localEndPointSource provides the handler with access to the local node's identity
type localEndPointSource struct {
	service *Service
}

// GetLocalEndPoint returns the local node's enode record, used for identifying this sentry to peers
func (s localEndPointSource) GetLocalEndPoint() *enode.Node {
	return s.service.p2pServer.Self()
}

// newService initializes the internal state of the gossip service
func newService(config Config, localId enode.ID, networkId uint64, genisis common.Hash, url string, datadir string) (*Service, error) {
	svc := &Service{
		config:   config,
		Instance: logger.New("gossip-service"),
		txCh:     make(chan *types.Transaction, 16384), // Buffered channel for transaction sniffing
	}

	// Initialize DNS discovery to bootstrap the peer list
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})
	var err error
	svc.operaDialCandidates, err = dnsclient.NewIterator(config.OperaDiscoveryURLs...)
	if err != nil {
		return nil, err
	}

	// newTxsHook is a callback passed to the handler. When the handler receives a valid
	// transaction from a peer, it calls this hook to pass the transaction up to the service.
	newTxsHook := func(tx *types.Transaction) {
		select {
		case svc.txCh <- tx:
		default:
			// Drop transaction if buffer is full to avoid blocking P2P layer
		}
	}

	// Create the core protocol handler which manages the 'opera' P2P protocol logic
	svc.handler, err = newHandler(handlerConfig{
		dataDir: datadir,

		networkID: networkId,
		config:    config,

		genesis: genisis,
		url:     url,

		localId:             localId,
		localEndPointSource: localEndPointSource{svc},
		newTxsHook:          newTxsHook,
	})
	if err != nil {
		return nil, err
	}

	rpc.SetExecutionTimeLimit(config.RPCTimeout)
	return svc, nil
}

type CleanupFunc func()

// Protocols returns protocols the service can communicate on.
func (s *Service) Protocols() ([]p2p.Protocol, CleanupFunc) {
	return MakeProtocols(s, s.handler, s.operaDialCandidates)
}

// MakeProtocols constructs the P2P protocol definitions for a  sentry node.
func MakeProtocols(svc *Service, backend *handler, disc enode.Iterator) ([]p2p.Protocol, func()) {
	// The FairMix combines different peer sources:
	// 1. Static/DNS Discovery (disc)
	// 2. Peers suggested by other peers via gossip (backend.GetSuggestedPeerIterator)
	//
	// Reducing the timeout makes the dialer rotate through candidates more quickly.
	mix := enode.NewFairMix(100 * time.Millisecond)
	if disc != nil {
		mix.AddSource(disc)
	}
	mix.AddSource(backend.GetSuggestedPeerIterator())

	nodeIter := mix
	closeIter := mix.Close

	// Support multiple versions of the protocol (e.g., Sonic 65, 64)
	protocols := make([]p2p.Protocol, len(ProtocolVersions))
	for i, version := range ProtocolVersions {
		version := version // Closure

		protocols[i] = p2p.Protocol{
			Name:    ProtocolName, // "opera"
			Version: version,
			Length:  protocolLengths[version],
			// Run is called by the p2p.Server when a peer connecting with this protocol is accepted
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				// Ensure the handler is fully initialized before processing peers
				backend.started.Wait()

				// Wrap the raw p2p.Peer in our protocol-specific peer object
				peer := newPeer(version, p, rw, backend.config.Protocol.PeerCache)
				defer peer.Close()

				select {
				case <-backend.quitSync:
					return p2p.DiscQuitting
				default:
					backend.wg.Add(1)
					defer backend.wg.Done()
					// Delegate the actual message loop and handshake to the handler
					return backend.handle(peer)
				}
			},
			NodeInfo: func() interface{} {
				// NodeInfo can be simplified or proxied if needed.
				return nil
			},
			PeerInfo: func(id enode.ID) interface{} {
				if p := backend.peers.Peer(id.String()); p != nil {
					return p.Info()
				}
				return nil
			},
			DialCandidates: nodeIter,
		}
	}
	return protocols, func() {
		if closeIter != nil {
			closeIter()
		}
	}
}

// APIs returns the RPC APIs that the  service offers.
func (s *Service) APIs() []rpc.API {
	// Expose 'eth' for transaction subscriptions and 'net' for peer stats
	return []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicAPI(&s.txFeed, s.handler),
			Public:    true,
		},
		{
			Namespace: "net",
			Version:   "1.0",
			Service:   NewPublicNetAPI(s.p2pServer, s.handler.networkID),
			Public:    true,
		},
	}
}

// Start is called after all services have been constructed and the networking
// layer was also started to allow the service to start doing its work.
func (s *Service) Start() error {
	// Start the background maintenance loops in the handler (discovery, stats, etc.)
	s.handler.Start(s.p2pServer.MaxPeers)

	// Start the transaction feed pump.
	// This goroutine listens on txCh for transactions "sniffed" from the network
	// and broadcasts them to local RPC subscribers via the event feed.
	go func() {
		for {
			select {
			case tx := <-s.txCh:
				// Send the transaction to the feed. The feed handles subscribers.
				// subscriber from stalling the entire P2P sniffing pipeline.
				s.txFeed.Send(tx)

			case <-s.handler.quitSync:
				return
			}
		}
	}()
	return nil
}

// Stop is called when the node is shutting down.
func (s *Service) Stop() error {
	defer log.Info("Sonic service stopped")

	// Stop all the peer-related stuff first.
	s.operaDialCandidates.Close()

	s.handler.Stop()
	// s.feed.Stop()

	return nil
}
