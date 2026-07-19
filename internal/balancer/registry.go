package balancer

import "fmt"

// Options carries algorithm parameters for NewByAlgorithm. All fields are
// optional; sensible defaults are applied where noted. Wrapper behaviors
// (priority tiers, slow start, outlier detection, zone awareness) are NOT
// applied here — the caller composes them explicitly with the New*Wrapper
// constructors so they can be stacked in the desired order.
type Options struct {
	// ConsistentHashReplicas is the number of virtual nodes per backend for the
	// consistent-hash ring. Defaults to 100 when <= 0.
	ConsistentHashReplicas int
	// ConsistentHashLoadFactor is the bounded-load multiplier over the average.
	// Defaults to 1.25 when <= 1.
	ConsistentHashLoadFactor float64
}

// NewByAlgorithm constructs a base Balancer for the given algorithm name. The
// recognized names mirror config.validAlgorithms:
//
//	round_robin          -> RoundRobin
//	least_conn           -> LeastConnections
//	weighted             -> WeightedRoundRobin (bursty, classic WRR)
//	swrr                 -> SWRR (smooth weighted round robin)
//	weighted_least_conn  -> WeightedLeastConn
//	weighted_random      -> WeightedRandom
//	p2c                  -> P2C (power-of-two-choices least connections)
//	consistent_hash      -> ConsistentHash (KeyedBalancer)
//	ewma                 -> EWMA (LatencyObserver)
//	ip_hash              -> IPHash (KeyedBalancer)
//
// It returns an error for unknown algorithms. Wrappers are applied by the caller.
func NewByAlgorithm(name string, opts Options) (Balancer, error) {
	switch name {
	case "round_robin":
		return NewRoundRobin(), nil
	case "least_conn":
		return NewLeastConnections(), nil
	case "weighted":
		return NewWeightedRoundRobin(), nil
	case "swrr":
		return NewSWRR(), nil
	case "weighted_least_conn":
		return NewWeightedLeastConn(), nil
	case "weighted_random":
		return NewWeightedRandom(), nil
	case "p2c":
		return NewP2C(), nil
	case "consistent_hash":
		return NewConsistentHash(opts.ConsistentHashReplicas, opts.ConsistentHashLoadFactor), nil
	case "ewma":
		return NewEWMA(), nil
	case "ip_hash":
		return NewIPHash(), nil
	default:
		return nil, fmt.Errorf("unknown load-balancing algorithm: %q", name)
	}
}
