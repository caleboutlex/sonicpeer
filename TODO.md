# SonicPeer TODO

## High Priority
- [ ] **Worker Pool for Feed**: While current goroutine spawning works, refactor to a worker pool for more deterministic resource usage during massive network floods.
- [ ] **Sidecar Reconnection**: Add robust `rpc.Dial` retry logic to the sidecar so it survives node restarts.
- [ ] **Dynamic Gas Policy**: Sync the Sentry's `MinGasPrice` with the backend's base fee automatically.

## Medium Priority
- [ ] **Peer Scorer**: Track unique transaction volume per peer and automatically rotate out peers that only provide redundant data.
- [ ] **GeoIP Heatmap**: Implement a tool to map the `nodes.json` population to identify geographic gaps in connectivity.

## Technical Debt
- [ ] **Semaphore Occupancy Gauge**: Add Prometheus metrics to track how often the `MsgsSemaphoreLimit` is saturated.
- [ ] **Unit Test Coverage**: Expand `tests/` to include edge-case validation for the RPC subscription filters.

## Performance Baseline
- **Average Discovery Lead**: +15ms
- **Connectivity Capacity**: 110+ active connections (Peak: 113)