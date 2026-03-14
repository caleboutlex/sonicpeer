# Changelog - March 14, 2026

## [Architectural Improvements]
- **Fast-Track Discovery**: Transaction and Hash messages now bypass the global semaphore, enabling "wire-speed" discovery independent of network load.
- **Clean Resource Management**: Removed legacy `sync.Pool` and `bufPool` implementations in favor of modern Go direct allocations, reducing complexity.
- **Lightweight Sentry Validation**: Implemented Signature (ECDSA) and `MinGasPrice` filtering. This ensures discovery metrics are "clean" and match standard mempool policies.

## [Connectivity & Discovery]
- **Aggressive Discovery**: Increased candidate dial throughput (8 -> 32) and increased collection frequency (60s -> 5s).
- **Topology Persistence**: Discovered nodes are now merged with connected peers and persisted to `nodes.json` for rapid "warm-start" connectivity.
- **Smart Neighborhood Maintenance**: Refactored peer-info requests to poll random subsets, reducing outbound noise while maintaining topology awareness.

## [API & Sidecar]
- **Standardized net Namespace**: Fully implemented `net_peerCount`, `net_version`, and `net_listening` to comply with standard Ethereum/Sonic RPC expectations.
- **Standardized eth Subscriptions**: Corrected `newPendingTransactions` and `newFullPendingTransactions` to return standard hex hashes and full objects respectively.
- **Enhanced Sidecar**: Added peer count monitoring and real-time latency deltas for comparative analysis.

## [Stability]
- **Epoch Guard**: Implemented an Epoch synchronization check with a zero-lock optimization path for synced peers and a configurable grace period for lagging peers.
- **Buffered Pump**: Increased internal transaction buffer to 16,384 and implemented a non-blocking feed pump to handle 5k+ tx bursts.