package arbitrage

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// BenchmarkRouterHostDecision measures the cached request-path cost of selecting
// an upstream target. RIC-2458 requires < 5ms on the cache-hit path; the real
// number is two orders of magnitude lower (a read lock over an immutable map),
// so this benchmark is a regression guard, not a tuning target.
func BenchmarkRouterHostDecision(b *testing.B) {
	now := time.Now()
	store := NewStore()
	// A realistic Enterprise catalog: many models, one decision each.
	items := make([]Decision, 0, 256)
	for i := 0; i < 256; i++ {
		items = append(items, Decision{
			Model:    "model-" + string(rune('a'+i%26)) + "-" + itoa(i),
			Provider: "aliyun",
			Endpoint: "https://aliyun.example.com",
			AuthRef:  "aliyun-ref",
		})
	}
	store.Apply(items, now, 30, now, 3)
	host := NewRouterHost(store, fixedClock(now), func(d Decision) string { return d.Provider })

	request := pluginapi.ModelRouteRequest{RequestedModel: "model-a-0"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, ok := host.RouteModel(ctx, request)
		if !ok || !resp.Handled {
			b.Fatal("expected a handled route on the cache-hit path")
		}
	}
}

// BenchmarkRouterHostMiss measures the fall-through path (no decision), which is
// what an unconfigured model pays on every request.
func BenchmarkRouterHostMiss(b *testing.B) {
	now := time.Now()
	store := NewStore()
	store.Apply([]Decision{{Model: "known", Provider: "aliyun"}}, now, 30, now, 3)
	host := NewRouterHost(store, fixedClock(now), func(d Decision) string { return d.Provider })

	request := pluginapi.ModelRouteRequest{RequestedModel: "unknown-model"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := host.RouteModel(ctx, request); ok {
			b.Fatal("an unknown model must fall through")
		}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [8]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}
