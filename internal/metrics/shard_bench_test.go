package metrics

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// BenchmarkAtomicCounter measures N goroutines all hammering a single
// atomic.Int64 (the baseline that suffers from false sharing at high
// GOMAXPROCS).
func BenchmarkAtomicCounter(b *testing.B) {
	for _, procs := range []int{1, 4, 8} {
		procs := procs
		b.Run(runtime.Version()+"_GOMAXPROCS_"+itoa(procs), func(b *testing.B) {
			prev := runtime.GOMAXPROCS(procs)
			defer runtime.GOMAXPROCS(prev)

			var counter atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					counter.Add(1)
				}
			})
			b.StopTimer()
			_ = counter.Load()
		})
	}
}

// BenchmarkShardedCounter measures the same workload using ShardedCounter.
// At GOMAXPROCS > 1 the padded shards eliminate false sharing and throughput
// should scale closer to linearly.
func BenchmarkShardedCounter(b *testing.B) {
	for _, procs := range []int{1, 4, 8} {
		procs := procs
		b.Run(runtime.Version()+"_GOMAXPROCS_"+itoa(procs), func(b *testing.B) {
			prev := runtime.GOMAXPROCS(procs)
			defer runtime.GOMAXPROCS(prev)

			sc := NewShardedCounter()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					sc.Add(1)
				}
			})
			b.StopTimer()
			_ = sc.Load()
		})
	}
}

// BenchmarkShardedCounterContended explicitly spawns goroutines equal to
// GOMAXPROCS to show contention behaviour under a fixed goroutine count
// rather than letting testing.B choose parallelism.
func BenchmarkShardedCounterContended(b *testing.B) {
	for _, procs := range []int{1, 4, 8} {
		procs := procs
		b.Run("goroutines_"+itoa(procs), func(b *testing.B) {
			prev := runtime.GOMAXPROCS(procs)
			defer runtime.GOMAXPROCS(prev)

			sc := NewShardedCounter()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				wg.Add(procs)
				for g := 0; g < procs; g++ {
					go func() {
						defer wg.Done()
						sc.Add(1)
					}()
				}
				wg.Wait()
			}
			b.StopTimer()
		})
	}
}

// itoa is a minimal int-to-string helper to avoid importing fmt/strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// TestShardedCounterAdd verifies basic Add/Load semantics under concurrency.
func TestShardedCounterAdd(t *testing.T) {
	const goroutines = 64
	const perGoroutine = 1000

	sc := NewShardedCounter()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				sc.Add(1)
			}
		}()
	}
	wg.Wait()

	got := sc.Load()
	want := int64(goroutines * perGoroutine)
	if got != want {
		t.Errorf("ShardedCounter.Load() = %d, want %d", got, want)
	}
}

// TestShardedCounterNegativeDelta checks that subtraction works correctly.
func TestShardedCounterNegativeDelta(t *testing.T) {
	sc := NewShardedCounter()
	sc.Add(100)
	sc.Add(-30)
	if got := sc.Load(); got != 70 {
		t.Errorf("Load() after Add(100) Add(-30) = %d, want 70", got)
	}
}

// TestShardedCounterZero verifies a freshly created counter reads zero.
func TestShardedCounterZero(t *testing.T) {
	sc := NewShardedCounter()
	if got := sc.Load(); got != 0 {
		t.Errorf("new ShardedCounter.Load() = %d, want 0", got)
	}
}
