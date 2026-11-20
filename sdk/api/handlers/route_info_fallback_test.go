package handlers

import (
	"strings"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func timeEvent(provider, model, outcome string, status int, code, errMsg string) coreexecutor.RouteEvent {
	return coreexecutor.RouteEvent{
		Provider: provider,
		Model:    model,
		Outcome:  outcome,
		Status:   status,
		Code:     code,
		Error:    errMsg,
	}
}

// Scenario 1: configured fallbacks modelA -> modelB, modelA fails, modelB
// serves. The header must carry modelA->modelB plus a failure reason.
func TestOpenRouterFallbackScenario1FallbackServed(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"pA", "pB"}, false)
	collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "upstream 503"))
	collector.ReportEvent(timeEvent("pB", "modelB", coreexecutor.RouteOutcomeSuccess, 0, "", ""))

	value := collector.OpenRouterFallback()
	if !strings.HasPrefix(value, "modelA->modelB") {
		t.Fatalf("fallback header = %q, want prefix modelA->modelB", value)
	}
	if !strings.Contains(value, "reason: upstream_503") {
		t.Fatalf("fallback header = %q, want reason upstream_503", value)
	}
}

// Scenario 2: every fallback model fails; no serving success. The header must
// be absent (the error body carries the final error).
func TestOpenRouterFallbackScenario2AllFailed(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"pA", "pB"}, false)
	collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "upstream overloaded"))
	collector.ReportEvent(timeEvent("pB", "modelB", coreexecutor.RouteOutcomeFailed, 429, "upstream_429", "rate limited"))

	if value := collector.OpenRouterFallback(); value != "" {
		t.Fatalf("all-failed fallback header = %q, want empty", value)
	}
}

// Same model served by a different provider after a failure is a genuine
// fallback: the header names the provider move.
func TestOpenRouterFallbackProviderChangeEmitted(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"providerA", "providerB"}, false)
	collector.ReportEvent(timeEvent("providerA", "modelX", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "down"))
	collector.ReportEvent(timeEvent("providerB", "modelX", coreexecutor.RouteOutcomeSuccess, 0, "", ""))

	value := collector.OpenRouterFallback()
	if !strings.HasPrefix(value, "providerA->providerB") {
		t.Fatalf("provider-change fallback header = %q, want prefix providerA->providerB", value)
	}
	if !strings.Contains(value, "reason: upstream_503") {
		t.Fatalf("provider-change fallback header = %q, want reason", value)
	}
}

// Scenario 3: no fallback configured; direct success. The header must be
// absent.
func TestOpenRouterFallbackScenario3NoFallbackConfig(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"pA"}, false)
	collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeSuccess, 0, "", ""))

	if value := collector.OpenRouterFallback(); value != "" {
		t.Fatalf("no-fallback header = %q, want empty", value)
	}
}

// Same-model internal credential retry (failure + success on the same model
// and provider) is not a routing fallback; the header must be absent.
func TestOpenRouterFallbackSameTargetCredentialRetryAbsent(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"pA"}, false)
	collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "transient"))
	collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeSuccess, 0, "", ""))

	if value := collector.OpenRouterFallback(); value != "" {
		t.Fatalf("same-target retry header = %q, want empty", value)
	}
}

// Scenario 4: concurrent reporting must not race or lose the marker. The
// collector must stay consistent under concurrent ReportEvent of a fallback
// and the header must still be emitted.
func TestOpenRouterFallbackScenario4ConcurrentConsistency(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"pA", "pB"}, false)
	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func() {
			collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "overloaded"))
			collector.ReportEvent(timeEvent("pB", "modelB", coreexecutor.RouteOutcomeSuccess, 0, "", ""))
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
	value := collector.OpenRouterFallback()
	if !strings.HasPrefix(value, "modelA->modelB") {
		t.Fatalf("concurrent fallback header = %q, want prefix modelA->modelB", value)
	}
}

// Header safety: a model name or error containing CR/LF must not inject lines.
func TestOpenRouterFallbackHeaderSafe(t *testing.T) {
	collector := NewRouteInfoCollector([]string{"pA", "pB"}, false)
	collector.ReportEvent(timeEvent("pA", "modelA\r\nX-Evil: 1", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "bad"))
	collector.ReportEvent(timeEvent("pB", "modelB", coreexecutor.RouteOutcomeSuccess, 0, "", ""))

	value := collector.OpenRouterFallback()
	if strings.Contains(value, "\r") || strings.Contains(value, "\n") {
		t.Fatalf("fallback header contains newline: %q", value)
	}
}

// Nil collector is inert.
func TestOpenRouterFallbackNilCollector(t *testing.T) {
	var collector *RouteInfoCollector
	if value := collector.OpenRouterFallback(); value != "" {
		t.Fatalf("nil collector header = %q, want empty", value)
	}
	collector.logFallbackIfAny() // must not panic
}

// attachOpenRouterFallback allocates the header map only when there is a
// value to emit, and does not clobber existing headers.
func TestAttachOpenRouterFallback(t *testing.T) {
	collector := NewRouteInfoCollector(nil, false)
	collector.ReportEvent(timeEvent("pA", "modelA", coreexecutor.RouteOutcomeFailed, 503, "upstream_503", "down"))
	collector.ReportEvent(timeEvent("pB", "modelB", coreexecutor.RouteOutcomeSuccess, 0, "", ""))

	headers := attachOpenRouterFallback(nil, collector)
	if headers == nil {
		t.Fatal("attachOpenRouterFallback returned nil headers for a fallback")
	}
	if v := headers.Get(OpenRouterFallbackHeader); v == "" {
		t.Fatal("fallback header not attached")
	}

	// No-fallback path must not allocate.
	headers = attachOpenRouterFallback(nil, NewRouteInfoCollector(nil, false))
	if headers != nil {
		t.Fatalf("no-fallback attach allocated headers: %v", headers)
	}
}
