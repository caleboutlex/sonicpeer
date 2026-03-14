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
	config              Config
	handler             *handler // the P2P protocol handler
	operaDialCandidates enode.Iterator

	stack *node.Node

	txFeed event.Feed
	txCh   chan *types.Transaction

	logger.Instance
}

// NewService creates a new  gossip service.
func NewService(stack *node.Node, config Config, datadir string, url string, genesis common.Hash, networkId uint64) (*Service, error) {
	localNodeId := enode.PubkeyToIDV4(&stack.Server().PrivateKey.PublicKey)
	svc, err := newService(config, localNodeId, networkId, genesis, url, datadir)
	if err != nil {
		return nil, err
	}

	svc.stack = stack
	return svc, nil
}

type localEndPointSource struct {
	service *Service
}

func (s localEndPointSource) GetLocalEndPoint() *enode.Node {
	return s.service.stack.Server().LocalNode().Node()
}

func newService(config Config, localId enode.ID, networkId uint64, genisis common.Hash, url string, datadir string) (*Service, error) {
	svc := &Service{
		config:   config,
		Instance: logger.New("gossip-service"),
		txCh:     make(chan *types.Transaction, 16384),
	}

	// init dialCandidates
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})
	var err error
	svc.operaDialCandidates, err = dnsclient.NewIterator(config.OperaDiscoveryURLs...)
	if err != nil {
		return nil, err
	}

	newTxsHook := func(tx *types.Transaction) {
		select {
		case svc.txCh <- tx:
		default:
			// Drop transaction if buffer is full to avoid blocking P2P layer
		}
	}

	// create protocol manager
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
	// Reduce the timeout from 1s to 100ms to make the dialer more aggressive
	mix := enode.NewFairMix(100 * time.Millisecond)
	if disc != nil {
		mix.AddSource(disc)
	}
	mix.AddSource(backend.GetSuggestedPeerIterator())

	nodeIter := mix
	closeIter := mix.Close

	protocols := make([]p2p.Protocol, len(ProtocolVersions))
	for i, version := range ProtocolVersions {
		version := version // Closure

		protocols[i] = p2p.Protocol{
			Name:    ProtocolName,
			Version: version,
			Length:  protocolLengths[version],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				// wait until the backend handler has started
				backend.started.Wait()
				peer := newPeer(version, p, rw, backend.config.Protocol.PeerCache)
				defer peer.Close()

				select {
				case <-backend.quitSync:
					return p2p.DiscQuitting
				default:
					backend.wg.Add(1)
					defer backend.wg.Done()
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
	return []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicAPI(&s.txFeed, s.handler),
			Public:    true,
		},
	}
}

// Start is called after all services have been constructed and the networking
// layer was also started to allow the service to start doing its work.
func (s *Service) Start() error {
	srv := s.stack.Server()
	s.handler.Start(srv.MaxPeers)

	// Start the transaction feed pump
	go func() {
		for {
			select {
			case tx := <-s.txCh:
				// Use a non-blocking send or a goroutine to prevent a slow RPC
				// subscriber from stalling the entire P2P sniffing pipeline.
				go s.txFeed.Send(tx)
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
