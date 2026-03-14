package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	SniffLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sonicpeer_tx_sniff_latency_seconds",
		Help:    "Latency between receiving a transaction and adding it to the local cache.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 2, 15), // 10us to ~160ms
	})
	PropagationLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sonicpeer_tx_propagation_latency_seconds",
		Help:    "Latency between receiving a transaction and completing the p2p send.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 15), // 100us to ~1.6s
	})
	QueueWaitLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sonicpeer_tx_queue_wait_seconds",
		Help:    "Time a transaction spent in the outbound peer queue.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 15),
	})
	SemaphoreWaitLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sonicpeer_semaphore_wait_seconds",
		Help:    "Time spent waiting to acquire the global message semaphore.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 15), // 100us to ~1.6s
	})
)
