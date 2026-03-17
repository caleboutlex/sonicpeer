package gossip

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/0xsoniclabs/sonic/config"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// Sentry is the main object for the gossip agent. It encapsulates the P2P stack and the gossip service.
// It is responsible for managing the lifecycle of the entire node.
type Sentry struct {
	stack   *node.Node // The base Ethereum-style node stack (P2P, RPC, etc.)
	svc     *Service   // The custom Sonic gossip service
	cleanup func()     // Cleanup function to tear down protocols and the stack
}

// SentryConfig encapsulates all the configuration for the sentry node.
type SentryConfig struct {
	// Backend validator settings
	BackendURL string

	// P2P network settings
	NetworkID  uint64
	Genesis    common.Hash
	DataDir    string
	Bootnodes  []string
	ListenPort int
	MaxPeers   int

	// Metrics and Profiling
	MetricsEnabled bool
	MetricsAddr    string
	MetricsPort    int

	// Sentry's own RPC server settings
	HTTPEnabled      bool
	HTTPListenAddr   string
	HTTPPort         int
	HTTPCORSDomain   []string
	HTTPVirtualHosts []string
	HTTPApi          []string
	HTTPPathPrefix   string
	WSEnabled        bool
	WSListenAddr     string
	WSPort           int
	WSApi            []string
	WSAllowedOrigins []string
	WSPathPrefix     string
	IPCDisabled      bool

	// Gossip protocol settings
	GossipConfig Config
}

// NewSentry creates and configures a new Sentry node, but does not start it.
func NewSentry(cfg SentryConfig) (*Sentry, error) {
	// 1. Initialize the base node configuration with Sonic defaults
	nodeCfg := config.DefaultNodeConfig()
	nodeCfg.DataDir = cfg.DataDir
	nodeCfg.P2P.ListenAddr = fmt.Sprintf(":%d", cfg.ListenPort)

	// Apply peer limit. If 0, DefaultNodeConfig's value is preserved to avoid
	// falling back to the p2p server's internal hardcoded defaults.
	if cfg.MaxPeers > 0 {
		nodeCfg.P2P.MaxPeers = cfg.MaxPeers
	}
	log.Info("Configuring P2P peer limit", "maxPeers", nodeCfg.P2P.MaxPeers)

	// Explicitly disable NAT; Sentry nodes are expected to be on public IPs
	nodeCfg.P2P.NAT = nil

	// Map SentryConfig RPC settings to the underlying node.Config fields
	nodeCfg.HTTPHost = cfg.HTTPListenAddr
	nodeCfg.HTTPPort = cfg.HTTPPort
	nodeCfg.HTTPCors = cfg.HTTPCORSDomain
	nodeCfg.HTTPVirtualHosts = cfg.HTTPVirtualHosts
	nodeCfg.HTTPModules = cfg.HTTPApi
	nodeCfg.HTTPPathPrefix = cfg.HTTPPathPrefix
	nodeCfg.WSHost = cfg.WSListenAddr
	nodeCfg.WSPort = cfg.WSPort
	nodeCfg.WSModules = cfg.WSApi
	nodeCfg.WSOrigins = cfg.WSAllowedOrigins
	nodeCfg.WSPathPrefix = cfg.WSPathPrefix

	if !cfg.HTTPEnabled {
		nodeCfg.HTTPHost = ""
	}
	if !cfg.WSEnabled {
		nodeCfg.WSHost = ""
	}
	if cfg.IPCDisabled {
		nodeCfg.IPCPath = ""
	}

	// Fix IPC path length issue: Unix sockets have a limit of ~104 chars.
	// If the path is too long, we relocate it to /tmp to prevent startup errors.
	ipcPath := nodeCfg.IPCPath
	if ipcPath == "" {
		ipcPath = "sonic.ipc"
	}
	if !filepath.IsAbs(nodeCfg.DataDir) {
		if cwd, err := os.Getwd(); err == nil {
			nodeCfg.DataDir = filepath.Join(cwd, nodeCfg.DataDir)
		}
	}
	if !filepath.IsAbs(ipcPath) {
		ipcPath = filepath.Join(nodeCfg.DataDir, ipcPath)
	}
	if len(ipcPath) > 100 {
		shortPath := filepath.Join(os.TempDir(), fmt.Sprintf("sonic-%d.ipc", os.Getpid()))
		log.Warn("IPC path too long, relocating to temporary path", "old", ipcPath, "new", shortPath)
		nodeCfg.IPCPath = shortPath
	}

	// 2. Parse and configure bootnodes for peer discovery
	var nodes []*enode.Node
	bootnodeURLs := cfg.Bootnodes
	if len(bootnodeURLs) == 0 {
		log.Info("Using default bootnodes", "network", "sonic")
		bootnodeURLs = config.Bootnodes["sonic"] // Fallback to hardcoded network bootnodes
	}
	for _, url := range bootnodeURLs {
		if url == "" {
			continue
		}
		n, err := enode.Parse(enode.ValidSchemes, url)
		if err != nil {
			return nil, fmt.Errorf("invalid bootnode url %q: %w", url, err)
		}
		nodes = append(nodes, n)
	}
	nodeCfg.P2P.BootstrapNodes = nodes

	// For a sentry, it's crucial to maintain persistent connections to bootnodes.
	// Adding them as static nodes ensures the p2p server will actively try to
	// connect and reconnect to them. This is more robust than relying on
	// discovery alone, especially on a fresh start with a clean data directory.
	if len(nodes) > 0 {
		nodeCfg.P2P.StaticNodes = nodes
	}

	// 3. Create the base Ethereum network stack
	stack, err := node.New(&nodeCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create p2p stack: %w", err)
	}

	cleanupStack := func() {
		if err := stack.Close(); err != nil && err != node.ErrNodeStopped {
			log.Warn("Failed to close stack", "err", err)
		}
	}

	// 4. Instantiate the Gossip Service that handles the 'opera' protocol
	svc, err := NewService(stack, cfg.GossipConfig, cfg.DataDir, cfg.BackendURL, cfg.Genesis, cfg.NetworkID)
	if err != nil {
		cleanupStack()
		return nil, fmt.Errorf("failed to create gossip service: %w", err)
	}

	// 5. Register the service with the stack's lifecycle and networking
	protocols, svcCleanup := svc.Protocols()
	stack.RegisterProtocols(protocols) // Add "opera" sub-protocol to P2P server
	stack.RegisterAPIs(svc.APIs())     // Add 'eth' and 'net' RPC namespaces
	stack.RegisterLifecycle(svc)       // Hook Service.Start() into Node.Start()

	sentry := &Sentry{
		stack: stack,
		svc:   svc,
		cleanup: func() {
			svcCleanup()
			cleanupStack()
		},
	}

	return sentry, nil
}

// Start starts the Sentry node's P2P server and services.
func (s *Sentry) Start() error {
	return s.stack.Start()
}

// Stop gracefully shuts down the Sentry node.
func (s *Sentry) Stop() {
	s.cleanup()
}

// Wait blocks until the node has fully shut down.
func (s *Sentry) Wait() {
	s.stack.Wait()
}

// NodeInfo returns information about the running P2P node.
func (s *Sentry) NodeInfo() *enode.Node {
	return s.stack.Server().LocalNode().Node()
}

// HTTPEndpoint returns the URL of the running HTTP RPC endpoint.
func (s *Sentry) HTTPEndpoint() string {
	return s.stack.HTTPEndpoint()
}
