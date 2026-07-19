package balancer

import (
	"fmt"
	"reverse-proxy-lb/internal/config"
	"testing"
)

func benchBackends(n int) []*Backend {
	bs := make([]*Backend, n)
	for i := 0; i < n; i++ {
		bs[i] = NewBackend(config.BackendConfig{
			URL:      fmt.Sprintf("http://10.0.0.%d:8080", i+1),
			Weight:   (i % 3) + 1,
			MaxConns: 1000,
		})
	}
	return bs
}

func benchNext(b *testing.B, mk func() Balancer) {
	for _, n := range []int{3, 16, 64} {
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			bal := mk()
			for _, be := range benchBackends(n) {
				bal.Add(be)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sel, err := bal.Next()
				if err == nil && sel != nil {
					sel.DecrConn()
				}
			}
		})
	}
}

func BenchmarkRoundRobinNext(b *testing.B) { benchNext(b, func() Balancer { return NewRoundRobin() }) }
func BenchmarkSWRRNext(b *testing.B)       { benchNext(b, func() Balancer { return NewSWRR() }) }
func BenchmarkP2CNext(b *testing.B)        { benchNext(b, func() Balancer { return NewP2C() }) }
func BenchmarkLeastConnNext(b *testing.B) {
	benchNext(b, func() Balancer { return NewLeastConnections() })
}
func BenchmarkWeightedRandomNext(b *testing.B) {
	benchNext(b, func() Balancer { return NewWeightedRandom() })
}

func BenchmarkConsistentHashNextForKey(b *testing.B) {
	for _, n := range []int{3, 16, 64} {
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			ch := NewConsistentHash(100, 1.25)
			for _, be := range benchBackends(n) {
				ch.Add(be)
			}
			keys := []string{"1.2.3.4", "5.6.7.8", "9.10.11.12", "203.0.113.9"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sel, err := ch.NextForKey(keys[i%len(keys)])
				if err == nil && sel != nil {
					sel.DecrConn()
				}
			}
		})
	}
}
