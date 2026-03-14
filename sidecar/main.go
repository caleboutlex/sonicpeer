// sidecar/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	deltaMetric = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sonicpeer_discovery_advantage_seconds",
		Help:    "Time difference (StandardNode_Arrival - SonicPeer_Arrival). Positive means SonicPeer was faster.",
		Buckets: prometheus.LinearBuckets(-0.5, 0.05, 20),
	}, []string{"cluster"})

	sonicOrphans = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sonicpeer_orphans_total",
		Help: "Total number of transactions seen only by SonicPeer within the tracking window.",
	})

	standardOrphans = promauto.NewCounter(prometheus.CounterOpts{
		Name: "standard_orphans_total",
		Help: "Total number of transactions seen only by the standard node within the tracking window.",
	})

	standardNodes []string = []string{
		"ws://localhost:18546",
	}

	sentryNode = "wss://localhost:8546"
)

type arrival struct {
	ts      time.Time
	isSonic bool
}

var (
	mu   sync.Mutex
	seen = make(map[common.Hash]arrival)

	// Internal counters for terminal reporting
	sonicOrphanCount    atomic.Uint64
	standardOrphanCount atomic.Uint64
)

func record(hash common.Hash, isSonic bool, cluster string) {
	mu.Lock()
	defer mu.Unlock()

	if first, ok := seen[hash]; ok {
		if first.isSonic == isSonic {
			return // Duplicate announcement from the same node type
		}
		var delta float64
		if first.isSonic {
			// SonicPeer arrived first, StandardNode arrived now. Positive delta.
			delta = time.Since(first.ts).Seconds()
		} else {
			// StandardNode arrived first, SonicPeer arrived now. Negative delta.
			delta = -time.Since(first.ts).Seconds()
		}
		deltaMetric.WithLabelValues(cluster).Observe(delta)
		log.Printf("[DELTA] Hash: %s | Advantage: %.4fs | Cluster: %s", hash.Hex()[:10], delta, cluster)
		delete(seen, hash)
	} else {
		seen[hash] = arrival{ts: time.Now(), isSonic: isSonic}
	}
}

func subscribe(ctx context.Context, url string, isSonic bool, cluster string) {
	client, err := rpc.Dial(url)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", url, err)
	}

	ch := make(chan common.Hash)
	sub, err := client.EthSubscribe(ctx, ch, "newPendingTransactions")
	if err != nil {
		log.Fatalf("Failed to subscribe to %s: %v", url, err)
	}
	defer sub.Unsubscribe()

	label := "Standard"
	if isSonic {
		label = "SonicPeer"
	}
	log.Printf("Subscribed to %s at %s", label, url)

	for {
		select {
		case hash := <-ch:
			record(hash, isSonic, cluster)
		case err := <-sub.Err():
			log.Printf("%s subscription error: %v", label, err)
			return
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start Prometheus exporter
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Println("Prometheus metrics available at :8080/metrics")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatalf("Metrics server failed: %v", err)
		}
	}()

	// Start subscriptions
	go subscribe(ctx, sentryNode, true, "mainnet") // SonicPeer
	for _, url := range standardNodes {
		go subscribe(ctx, url, false, "mainnet")
	}

	// Periodically clean up stale entries that only hit one node
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			mu.Lock()
			now := time.Now()
			tracked := len(seen)
			for h, a := range seen {
				if now.Sub(a.ts) > 30*time.Second {
					if a.isSonic {
						sonicOrphans.Inc()
						sonicOrphanCount.Add(1)
					} else {
						standardOrphans.Inc()
						standardOrphanCount.Add(1)
					}
					delete(seen, h)
				}
			}
			log.Printf("[STAT] Heartbeat | Sonic Orphans: %d | Std Orphans: %d | Tracking Map: %d",
				sonicOrphanCount.Load(), standardOrphanCount.Load(), tracked)
			mu.Unlock()
		}
	}()

	fmt.Println("Sidecar running. Monitoring discovery latency delta...")
	select {}
}
