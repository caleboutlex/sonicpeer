package gossip

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Fantom-foundation/lachesis-base/inter/dag"
	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/Fantom-foundation/lachesis-base/utils/cachescale"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/0xsoniclabs/sonic/eventcheck/heavycheck"
	"github.com/0xsoniclabs/sonic/gossip/filters"
	"github.com/0xsoniclabs/sonic/gossip/gasprice"
)

type (
	// ProtocolConfig is config for p2p protocol
	ProtocolConfig struct {
		// 0/M means "optimize only for throughput", N/0 means "optimize only for latency", N/M is a balanced mode

		LatencyImportance    int
		ThroughputImportance int

		MsgsSemaphoreLimit   dag.Metric
		MsgsSemaphoreTimeout time.Duration

		ProgressBroadcastPeriod time.Duration

		MaxInitialTxHashesSend    int
		MaxRandomTxHashesSend     int
		RandomTxHashesSendPeriod  time.Duration
		PeerInfoCollectionPeriod  time.Duration
		PeerInfoMaintenancePeriod time.Duration
		PeerEndPointUpdatePeriod  time.Duration
		PeerStaleEpochGracePeriod time.Duration

		PeerCache PeerCacheConfig
	}

	// Config for the gossip service.
	Config struct {
		FilterAPI filters.Config

		// This can be set to list of enrtree:// URLs which will be queried
		// for nodes to connect to.
		OperaDiscoveryURLs []string

		TxIndex bool // Whether to enable indexing transactions and receipts or not

		// Protocol options
		Protocol ProtocolConfig

		HeavyCheck heavycheck.Config

		// Gas Price Oracle options
		GPO gasprice.Config

		// RPCGasCap is the global gas cap for eth-call variants.
		RPCGasCap uint64 `toml:",omitempty"`

		// RPCEVMTimeout is the global timeout for eth-call.
		RPCEVMTimeout time.Duration

		// RPCTxFeeCap is the global transaction fee(price * gaslimit) cap for
		// send-transction variants. The unit is ether.
		RPCTxFeeCap float64 `toml:",omitempty"`

		// RPCTimeout is a global time limit for RPC methods execution.
		RPCTimeout time.Duration

		// allows only for EIP155 transactions.
		AllowUnprotectedTxs bool

		// MaxResponseSize is a limit for maximum response size in some RPC calls in bytes
		MaxResponseSize int

		// StructLogLimit is a limit for maximum number of logs in structured EVM debug log
		StructLogLimit int

		RPCBlockExt bool

		SentryMode bool
	}
)

type PeerCacheConfig struct {
	MaxKnownTxs    int // Maximum transactions hashes to keep in the known list (prevent DOS)
	MaxKnownEvents int // Maximum event hashes to keep in the known list (prevent DOS)
	// MaxQueuedItems is the maximum number of items to queue up before
	// dropping broadcasts. This is a sensitive number as a transaction list might
	// contain a single transaction, or thousands.
	MaxQueuedItems idx.Event
	MaxQueuedSize  uint64
}

// DefaultConfig returns the default configurations for the gossip service.
func DefaultConfig(scale cachescale.Func) Config {
	cfg := Config{
		FilterAPI: filters.DefaultConfig(),

		TxIndex: true,

		HeavyCheck: heavycheck.DefaultConfig(),

		Protocol: ProtocolConfig{
			LatencyImportance:    100,
			ThroughputImportance: 0,
			MsgsSemaphoreLimit: dag.Metric{
				Num:  scale.Events(5000),
				Size: scale.U64(150 * opt.MiB),
			},

			MsgsSemaphoreTimeout:    10 * time.Second,
			ProgressBroadcastPeriod: 10 * time.Second,

			MaxInitialTxHashesSend:    20000,
			MaxRandomTxHashesSend:     250, // match softLimitItems to fit into one message
			RandomTxHashesSendPeriod:  1 * time.Second,
			PeerInfoCollectionPeriod:  5 * time.Second,
			PeerInfoMaintenancePeriod: 5 * time.Minute,
			PeerEndPointUpdatePeriod:  5 * time.Minute,
			PeerStaleEpochGracePeriod: 1 * time.Second,
			PeerCache:                 DefaultPeerCacheConfig(scale),
		},

		RPCEVMTimeout: 5 * time.Second,

		GPO: gasprice.Config{
			MaxGasPrice:      gasprice.DefaultMaxGasPrice,
			MinGasPrice:      new(big.Int),
			DefaultCertainty: 0.5 * gasprice.DecimalUnit,
		},

		RPCBlockExt: true,

		RPCGasCap:   50000000,
		RPCTxFeeCap: 100, // 100 FTM
		RPCTimeout:  5 * time.Second,

		MaxResponseSize: 25 * 1024 * 1024,
		StructLogLimit:  2000,
	}

	return cfg
}

func (c *Config) Validate() error {
	p := c.Protocol

	if p.MsgsSemaphoreLimit.Size < protocolMaxMsgSize {
		return fmt.Errorf("MsgsSemaphoreLimit.Size has to be at least %d", protocolMaxMsgSize)
	}

	return nil
}

func DefaultPeerCacheConfig(scale cachescale.Func) PeerCacheConfig {
	return PeerCacheConfig{
		MaxKnownTxs:    24576*3/4 + scale.I(24576/4),
		MaxKnownEvents: 24576*3/4 + scale.I(24576/4),
		MaxQueuedItems: 4096*3/4 + scale.Events(4096/4),
		MaxQueuedSize:  protocolMaxMsgSize*3/4 + 1024 + scale.U64(protocolMaxMsgSize/4),
	}
}
