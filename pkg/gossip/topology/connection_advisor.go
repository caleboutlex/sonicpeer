package topology

import (
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// ConnectionAdvisor is a utility that provides suggestions on which peers to
// connect to and which peers to disconnect from based on the local node's
// neighborhood. The advisor is used to maintain a healthy peer set and to
// optimize the local node's connectivity as well as the overall network topology.
type ConnectionAdvisor interface {
	// GetNewPeerSuggestion returns a new peer that should be connected to.
	GetNewPeerSuggestion() *enode.Node

	// GetRedundantPeerSuggestion returns the ID of a peer that should be
	// disconnected in favor of another peer if needed.
	GetRedundantPeerSuggestion() *enode.ID

	// UpdatePeers updates information about the peers connected to a given peer.
	// This information is used to obtain an overview on the local node's
	// neighborhood from which decisions changes in the peer set can be made.
	// The provided peer is assumed to be a peer of the local node.
	UpdatePeers(peer enode.ID, peers []*enode.Node)
}

func NewConnectionAdvisor(localId enode.ID) ConnectionAdvisor {
	return newConnectionAdvisor[enode.ID, *enode.Node](
		localId, 60*time.Second, func(n *enode.Node) enode.ID { return n.ID() },
	)
}

func newConnectionAdvisor[I comparable, R any](
	localId I,
	maxPeerInfoAge time.Duration,
	getId func(R) I,
) *connectionAdvisor[I, R] {
	return &connectionAdvisor[I, R]{
		neighborhood:   make(map[I]neighborhoodEntry[R]),
		localId:        localId,
		getId:          getId,
		maxPeerInfoAge: maxPeerInfoAge,
	}
}

// connectionAdvisor is a ConnectionAdvisor implementation that suggests new peers
// to connect to and redundant peers to disconnect from based on a simple heuristic.
// The implementation is generic to simplify testing. The type parameter I is the
// type of the peer ID and R is the type of the peer reference required to establish
// a connection.
type connectionAdvisor[I comparable, R any] struct {
	mu sync.Mutex

	// Keep track of the neighbors of each peer.
	neighborhood map[I]neighborhoodEntry[R]

	// The ID of the local node.
	localId I

	// maxPeerInfoAge is the maximum age of peer information that is considered
	// when making suggestions on adding or removing peers. Older information
	// is discarded.
	maxPeerInfoAge time.Duration

	// getId returns the ID of a stored peer reference
	getId func(R) I
}

type neighborhoodEntry[T any] struct {
	peers []T
	time  time.Time
}

func (c *connectionAdvisor[I, T]) GetNewPeerSuggestion() T {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Search for a peer of a peer that is not already connected to the local node.
	now := time.Now()
	// Use reservoir sampling (Algorithm R) to select a single random candidate
	// without allocating a slice to hold all of them. This is more memory-efficient.
	var candidate T
	var foundCandidate bool
	count := 0

	for peer, entry := range c.neighborhood {
		if now.Sub(entry.time) > c.maxPeerInfoAge {
			delete(c.neighborhood, peer)
			continue
		}
		for _, p := range entry.peers {
			id := c.getId(p)
			if id == c.localId {
				continue
			}
			if _, found := c.neighborhood[id]; !found {
				count++
				if rand.Intn(count) == 0 {
					candidate = p
					foundCandidate = true
				}
			}
		}
	}

	if foundCandidate {
		return candidate
	}

	var zero T // Return zero value if no candidates were found
	return zero
}

func (c *connectionAdvisor[I, T]) GetRedundantPeerSuggestion() *I {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Count the number of indirect connections to all peers and
	// recommend the one that has the most indirect connections.
	count := map[I]int{}
	now := time.Now()
	for peer, entry := range c.neighborhood {
		if now.Sub(entry.time) > c.maxPeerInfoAge {
			delete(c.neighborhood, peer)
			continue
		}
		for _, peer := range entry.peers {
			peerId := c.getId(peer)
			if _, found := c.neighborhood[peerId]; found {
				count[peerId]++
			}
		}
	}
	delete(count, c.localId)

	var maxCount int
	var maxPeers []I // Changed to a slice to handle ties
	for peer, c := range count {
		if c > maxCount {
			maxCount = c
			maxPeers = maxPeers[:0] // Reset slice
			maxPeers = append(maxPeers, peer)
		} else if c == maxCount && maxCount > 0 {
			maxPeers = append(maxPeers, peer)
		}
	}

	if len(maxPeers) == 0 {
		return nil
	}
	// Randomly select one from the most redundant peers to avoid deterministic behavior
	selectedPeer := maxPeers[rand.Intn(len(maxPeers))]
	return &selectedPeer
}

func (c *connectionAdvisor[I, T]) UpdatePeers(peer I, peers []T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.neighborhood[peer] = neighborhoodEntry[T]{
		peers: peers,
		time:  time.Now(),
	}
}
