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
type Sentry struct {
	stack   *node.Node
	svc     *Service
	cleanup func()
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
	// 1. Initialize node config with defaults and apply our config
	nodeCfg := config.DefaultNodeConfig()
	nodeCfg.DataDir = cfg.DataDir
	nodeCfg.P2P.ListenAddr = fmt.Sprintf(":%d", cfg.ListenPort)
	nodeCfg.P2P.MaxPeers = cfg.MaxPeers
	nodeCfg.P2P.NAT = nil // Explicitly disable NAT, sentry should have a public IP

	// RPC settings
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

	// If bootnodes are not explicitly set, use the sonic defaults.
	var nodes []*enode.Node
	bootnodeURLs := cfg.Bootnodes
	if len(bootnodeURLs) == 0 {
		log.Info("Using default bootnodes", "network", "sonic")
		bootnodeURLs = config.Bootnodes["sonic"]
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

	// 3. Create the base network stack.
	stack, err := node.New(&nodeCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create p2p stack: %w", err)
	}

	cleanupStack := func() {
		if err := stack.Close(); err != nil && err != node.ErrNodeStopped {
			log.Warn("Failed to close stack", "err", err)
		}
	}

	// 5. Instantiate the Service.
	svc, err := NewService(stack, cfg.GossipConfig, cfg.DataDir, cfg.BackendURL, cfg.Genesis, cfg.NetworkID)
	if err != nil {
		cleanupStack()
		return nil, fmt.Errorf("failed to create gossip service: %w", err)
	}

	// 6. Register the service's protocols and lifecycle with the node stack.
	protocols, svcCleanup := svc.Protocols()
	stack.RegisterProtocols(protocols)
	stack.RegisterAPIs(svc.APIs())
	stack.RegisterLifecycle(svc)

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
