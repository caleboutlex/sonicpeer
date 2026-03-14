# Changelog - March 14, 2026

## [Architectural Improvements]
- **Fast-Track Discovery**: Refactored `handler.go` to process transaction and hash announcements *before* acquiring the global message semaphore. This eliminated 60s+ latency spikes during network bursts.
- **Decoupled Metrics**: Centralized Prometheus collectors into `internal/metrics.go` and isolated sidecar metrics to keep business logic clean.
- **Resource Optimization**: Removed all `sync.Pool` and `bufPool` implementations in `handler.go` and `peer.go` in favor of direct allocations, simplifying memory management for modern Go runtimes.

## [Connectivity & Discovery]
- **Aggressive Peering**: Increased discovery candidate throughput from 8 to 32 and reduced collection periods from 60s to 5s.
- **Topology Persistence**: Updated `savePeers` to persist every discovered `enode` to `nodes.json`, enabling a "warm-start" capability that saturates connection slots instantly on restart.
- **Smart Maintenance**: Implemented immediate peer-list requests for new connections and a random-subset maintenance loop to reduce network chatter.
- **Stealth Mode**: Configured the node to ignore `GetPeerInfosMsg` and `GetEvmTxsMsg` to preserve outbound bandwidth for critical transaction propagation.

## [Validation & Reliability]
- **Lightweight Sentry Validation**: Added `types.Sender` (Signature/ChainID) and `MinGasPrice` filtering. This ensures the Sentry only records valid, actionable Sonic transactions, resulting in "clean" discovery metrics.
- **Epoch Synchronization**: Implemented `checkPeerStaleness` with a configurable grace period to ensure the node only gossips with peers on the correct Epoch.
- **Lock Optimization**: Optimized the peer synchronization path to use zero-lock "dirty reads" for synced peers, reducing CPU contention.

## [API & Monitoring]
- **Standardized RPC**: Fully implemented the `net` namespace (`net_peerCount`, `net_version`, `net_listening`) and the `eth` subscription namespace (`newPendingTransactions`, `newFullPendingTransactions`).
- **Sidecar Evolution**: Enhanced the terminal sidecar with real-time latency deltas, orphan tracking, and peer count monitoring for both Sentry and Standard nodes.
- **Feed Pump**: Instrumented a buffered transaction pump in `Service` to ensure high-frequency P2P discovery doesn't block on slow RPC subscribers.

## [Testing]
- **Integration Suite**: Created `tests/integration_test.go` to verify Sentry initialization, lifecycle, and RPC subscription functionality.
- **Unit Test Cleanup**: Streamlined `handler_test.go` by removing redundant benchmarks and focusing on critical DoS protection tests.