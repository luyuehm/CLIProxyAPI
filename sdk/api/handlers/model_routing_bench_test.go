package handlers

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// BenchmarkModelRoutingAllowedFunc pins the cost of the candidate predicate:
// it runs once per priced model on every routed request, so a regression here
// is a gateway-wide latency regression, not a local one.
//
// Reference (Apple M-series, 200 priced models, ~1k QPS scale): the
// registry-mutex path is ~100ns per model — ~20µs for a 200-model table. The
// earlier util.GetProviderName-based version measured ~50µs per request
// because it allocated and sorted a provider slice per model.
//
// Run: go test ./sdk/api/handlers/ -run XXX -bench BenchmarkModelRoutingAllowedFunc
func BenchmarkModelRoutingAllowedFunc(b *testing.B) {
	const clientID = "bench-model-routing"
	const modelCount = 200

	infos := make([]*registry.ModelInfo, 0, modelCount)
	names := make([]string, 0, modelCount)
	for i := 0; i < modelCount; i++ {
		name := fmt.Sprintf("bench-routing-model-%03d", i)
		infos = append(infos, &registry.ModelInfo{ID: name, Object: "model", OwnedBy: "claude"})
		names = append(names, name)
	}
	registry.GetGlobalRegistry().RegisterClient(clientID, "claude", infos)
	defer registry.GetGlobalRegistry().UnregisterClient(clientID)

	handler := &BaseAPIHandler{}
	allowed := handler.modelRoutingAllowedFunc()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, name := range names {
			if !allowed(name) {
				b.Fatalf("registered model %q reported unservable", name)
			}
		}
	}
}
