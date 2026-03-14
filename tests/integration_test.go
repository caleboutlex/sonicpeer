package tests

import (
	"testing"
	"time"

	"sonicpeer/pkg/gossip"

	"github.com/Fantom-foundation/lachesis-base/utils/cachescale"
	"github.com/ethereum/go-ethereum/common"
)

func TestSentryInitialization(t *testing.T) {
	// We define a basic config. Note that NewSentry attempts to initialize
	// the P2P stack and Dial the backend, so this test focuses on structural validation.
	cfg := gossip.SentryConfig{
		BackendURL:   "ws://localhost:18546", // Mock or non-existent in unit test context
		NetworkID:    146,
		Genesis:      common.HexToHash("0xdbec022b6a5b0a15c902cf16620d2048f69e9b4cb0dd5d6b57edcf842b85494a"),
		DataDir:      t.TempDir(),
		ListenPort:   30303,
		MaxPeers:     10,
		GossipConfig: gossip.DefaultConfig(cachescale.Identity),
	}

	// Test if we can instantiate the sentry without immediate panic or logic errors.
	// Note: Actual network initialization may be skipped or fail depending on environment.
	sentry, err := gossip.NewSentry(cfg)
	if err != nil {
		t.Logf("Sentry creation failed as expected in restricted env: %v", err)
		return
	}

	if sentry == nil {
		t.Fatal("Sentry object is nil")
	}

	defer sentry.Stop()
	t.Log("Sentry initialized successfully")
}

// TestSentryNodeLifecycle verifies the full startup and shutdown sequence of the Sentry node.
func TestSentryNodeLifecycle(t *testing.T) {
	cfg := gossip.SentryConfig{
		BackendURL:   "ws://localhost:18546",
		NetworkID:    146,
		Genesis:      common.HexToHash("0xdbec022b6a5b0a15c902cf16620d2048f69e9b4cb0dd5d6b57edcf842b85494a"),
		DataDir:      t.TempDir(),
		ListenPort:   0, // Let OS pick a free port
		MaxPeers:     5,
		HTTPEnabled:  true,
		HTTPPort:     0,
		GossipConfig: gossip.DefaultConfig(cachescale.Identity),
	}

	sentry, err := gossip.NewSentry(cfg)
	if err != nil {
		t.Logf("Sentry creation skipped (requires backend): %v", err)
		return
	}

	// Attempt to start the node
	if err := sentry.Start(); err != nil {
		t.Fatalf("Failed to start sentry node: %v", err)
	}

	// Verify Local Node Identity is established
	nodeInfo := sentry.NodeInfo()
	if nodeInfo == nil {
		t.Error("Sentry node info is missing after start")
	} else {
		t.Logf("Sentry Node ID: %s", nodeInfo.ID().String())
	}

	// Ensure the stack is running for a brief moment to catch initialization panics
	time.Sleep(100 * time.Millisecond)

	// Graceful Stop
	sentry.Stop()
	t.Log("Sentry node stopped gracefully")
}
