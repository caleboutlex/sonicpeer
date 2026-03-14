# SonicPeer TODO

## High Priority
- [ ] **Worker Pool for Feed**: Refactor the transaction pump in `service.go` to use a worker pool instead of spawning a goroutine per transaction during 5k+ bursts.
- [ ] **Sidecar Reconnection**: Implement a robust reconnection loop in `sidecar/main.go` to handle Sentry or Standard node restarts without crashing.
- [ ] **Dynamic Gas Policy**: Implement a mechanism to sync the Sentry's `MinGasPrice` with the backend full node's base fee to ensure filtering remains accurate.

## Medium Priority
- [ ] **Peer Scorer**: Add a scoring system to `peerset.go` to automatically disconnect from peers that consistently send duplicate or invalid transactions.
- [ ] **GeoIP Analysis**: Create a utility in `cmd/` to analyze `nodes.json` and map the geographic distribution of the Sonic network for optimal Sentry placement.
- [ ] **Health Check API**: Add a `sonic_health` RPC method to verify the end-to-end connectivity between the Sentry and the backend validator.

## Technical Debt
- [ ] **Semaphore Metrics**: Add a gauge to track current semaphore occupancy to fine-tune `MsgsSemaphoreLimit` in `config.go`.
- [ ] **Mock Backend**: Improve integration tests by creating a mock RPC backend to remove the dependency on a running Sonic node for CI/CD.

## Observations
*Current Discovery Performance:* Consistent 10ms-30ms advantage over standard nodes.
*Current Connectivity:* Successfully discovering 350+ nodes; focus on breaking the 60-connection plateau via port forwarding and static peering.
*Data Quality:* Sonic Orphans are currently 0 following the implementation of signature validation—this is the "honest" baseline.