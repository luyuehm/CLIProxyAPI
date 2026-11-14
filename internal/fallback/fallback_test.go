package fallback_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/fallback"
)

// fakeExec is a scripted execution table keyed by provider name.
type fakeExec struct {
	// errors is the error each provider returns when attempted.
	errors map[string]error
	// calls records attempt order as "provider:model" entries.
	calls []string
	// executed counts total attempts.
	executed int
}

func (f *fakeExec) run(ctx context.Context, provider, model string) error {
	f.executed++
	f.calls = append(f.calls, provider)
	if f.errors == nil {
		return nil
	}
	return f.errors[provider]
}

func httpErr(provider string, status int) error {
	return &fallback.ProviderError{Provider: provider, Status: status,
		Err: errors.New("upstream http " + itoa(status))}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func boolPtr(b bool) *bool { return &b }

// Scenario 1: preferred provider 503 → auto-forward to B → 200, and the log
// record carries fallback_reason=provider_A_503.
func TestScenario1PreferredFailureFallsBack(t *testing.T) {
	exec := &fakeExec{errors: map[string]error{"A": httpErr("A", 503)}}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A"},
			{Provider: "B"},
		},
		CostLimit: 0.01, // both candidates have unknown cost → admitted
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "model-x", fallback.RequestOptions{}, exec.run)

	if !out.Success {
		t.Fatalf("outcome.Success = false, want true (B must serve): %v", out.Err)
	}
	if out.Provider != "B" {
		t.Fatalf("final provider = %q, want B", out.Provider)
	}
	if len(exec.calls) != 2 || exec.calls[0] != "A" || exec.calls[1] != "B" {
		t.Fatalf("attempt order = %v, want [A B]", exec.calls)
	}
	// The observability record must carry the stable fallback_reason code.
	if len(out.Fallbacks) == 0 {
		t.Fatal("no FallbackRecord emitted for the A→B switch")
	}
	rec := out.Fallbacks[len(out.Fallbacks)-1]
	if rec.Reason != "provider_A_503" {
		t.Fatalf("fallback_reason = %q, want provider_A_503", rec.Reason)
	}
	if rec.From != "A" || rec.To != "B" {
		t.Fatalf("record %q→%q, want A→B", rec.From, rec.To)
	}
}

// Scenario 2: empty fallback chain → the raw preferred error is returned, no
// fallback is attempted.
func TestScenario2EmptyChainReturnsRawError(t *testing.T) {
	raw := httpErr("A", 500)
	exec := &fakeExec{errors: map[string]error{"A": raw}}
	// Candidates carries only the preferred provider; the fallback list
	// (candidates[1:]) is empty.
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}},
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "model-x", fallback.RequestOptions{}, exec.run)

	if out.Success {
		t.Fatal("outcome.Success = true, want false")
	}
	if !errors.Is(out.Err, raw) {
		t.Fatalf("final error = %v, want the raw preferred error (pointer identity)", out.Err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no fallback when the chain is empty)", len(exec.calls))
	}
	if len(out.Fallbacks) != 1 || out.Fallbacks[0].Reason != "provider_A_500" {
		t.Fatalf("records = %+v, want the single raw-error record", out.Fallbacks)
	}
}

// Scenario 3: every candidate's known estimated cost exceeds cost_limit → the
// request fails with cost_limit_exceeded and no provider is called.
func TestScenario3CostLimitIsHard(t *testing.T) {
	exec := &fakeExec{}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A", CostUSD: 0.05},
			{Provider: "B", CostUSD: 0.10},
		},
		CostLimit: 0.01,
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "model-x", fallback.RequestOptions{}, exec.run)

	if out.Success {
		t.Fatal("outcome.Success = true, want false")
	}
	if !errors.Is(out.Err, fallback.ErrCostLimitExceeded) {
		t.Fatalf("final error = %v, want ErrCostLimitExceeded", out.Err)
	}
	if exec.executed != 0 {
		t.Fatalf("attempts = %d, want 0 (hard constraint must not execute)", exec.executed)
	}
	if len(out.Fallbacks) == 0 || !strings.Contains(out.Fallbacks[0].Reason, "cost_limit_exceeded") {
		t.Fatalf("records = %+v, want a cost_limit_exceeded record", out.Fallbacks)
	}
}

// Scenario 4: request-level fallback_enabled=false → even with a full chain,
// the preferred error is returned directly.
func TestScenario4RequestLevelDisable(t *testing.T) {
	raw := httpErr("A", 429)
	exec := &fakeExec{errors: map[string]error{"A": raw}}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A"},
			{Provider: "B"},
		},
	}
	disabled := boolPtr(false)
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "model-x", fallback.RequestOptions{Fallback: disabled}, exec.run)

	if out.Success {
		t.Fatal("outcome.Success = true, want false")
	}
	if !errors.Is(out.Err, raw) {
		t.Fatalf("final error = %v, want the raw preferred error", out.Err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != "A" {
		t.Fatalf("attempts = %v, want exactly [A] (fallback must be skipped)", exec.calls)
	}
}

// B1 regression: with the fallback switch off, a preferred candidate that is
// itself excluded by a hard constraint must be the terminal outcome — never a
// silent promotion to the next candidate.
func TestDisableDoesNotPromoteOverFilteredPreferred(t *testing.T) {
	// Quality floor excludes preferred A (quality 1.0 < 2.0). The user opted
	// out of fallback, so B must not be served.
	disabled := boolPtr(false)
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A", Quality: 1.0},
			{Provider: "B", Quality: 3.0},
		},
		QualityFloor: 2.0,
	}
	executed := 0
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{Fallback: disabled},
		func(ctx context.Context, provider, model string) error {
			executed++
			return nil
		})
	if executed != 0 {
		t.Fatalf("attempts = %d, want 0 (B must not be promoted)", executed)
	}
	if !errors.Is(out.Err, fallback.ErrQualityFloorUnsat) {
		t.Fatalf("final error = %v, want ErrQualityFloorUnsat", out.Err)
	}

	// Same shape with the cost limit: preferred A is over the limit.
	policy2 := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A", CostUSD: 0.05},
			{Provider: "B", CostUSD: 0.001},
		},
		CostLimit: 0.01,
	}
	o2 := fallback.NewOrchestrator(policy2, nil)
	out2 := o2.Run(context.Background(), "m", fallback.RequestOptions{Fallback: disabled},
		func(ctx context.Context, provider, model string) error {
			t.Fatalf("B must not be executed")
			return nil
		})
	if !errors.Is(out2.Err, fallback.ErrCostLimitExceeded) {
		t.Fatalf("final error = %v, want ErrCostLimitExceeded", out2.Err)
	}
}

// A request without the switch (nil) follows the global chain — sanity check
// that the default is enabled, not disabled.
func TestScenario4bNilSwitchFollowsChain(t *testing.T) {
	exec := &fakeExec{errors: map[string]error{"A": httpErr("A", 503)}}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if !out.Success || out.Provider != "B" {
		t.Fatalf("nil switch must follow the chain: %+v", out)
	}
}

// Quality floor: candidates whose known quality sits below the floor are
// excluded before execution.
func TestQualityFloorExcludesCandidates(t *testing.T) {
	exec := &fakeExec{}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A", Quality: 1.0},
			{Provider: "B", Quality: 3.0},
		},
		QualityFloor: 2.0,
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)

	if !out.Success || out.Provider != "B" {
		t.Fatalf("quality floor must select B: %+v", out)
	}
	if len(exec.calls) != 1 || exec.calls[0] != "B" {
		t.Fatalf("attempts = %v, want exactly [B]", exec.calls)
	}
}

// A chain where every candidate is below the floor → terminal error.
func TestQualityFloorUnsat(t *testing.T) {
	exec := &fakeExec{}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{
			{Provider: "A", Quality: 1.0},
			{Provider: "B", Quality: 1.5},
		},
		QualityFloor: 2.0,
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if !errors.Is(out.Err, fallback.ErrQualityFloorUnsat) {
		t.Fatalf("final error = %v, want ErrQualityFloorUnsat", out.Err)
	}
	if exec.executed != 0 {
		t.Fatalf("attempts = %d, want 0", exec.executed)
	}
}

// Timeout budget: no attempt may start once the budget elapses. A scripted
// clock jumps past the budget after the first attempt.
func TestTimeoutBudget(t *testing.T) {
	exec := &fakeExec{errors: map[string]error{"A": httpErr("A", 503)}}
	var now time.Time = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	o := fallback.NewOrchestrator(fallback.Policy{
		Candidates:    []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
		TimeoutBudget: 5 * time.Second,
	}, nil)
	o.Now = func() time.Time {
		// Jump past the budget after the first attempt so the second one is
		// refused by the gate.
		if exec.executed >= 1 {
			now = now.Add(10 * time.Second)
		}
		return now
	}
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if out.Success {
		t.Fatal("outcome.Success = true, want false")
	}
	if !errors.Is(out.Err, fallback.ErrTimeoutBudgetExhausted) {
		t.Fatalf("final error = %v, want ErrTimeoutBudgetExhausted", out.Err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("attempts = %v, want exactly [A]", exec.calls)
	}
	if len(out.Fallbacks) == 0 || out.Fallbacks[len(out.Fallbacks)-1].Reason != "timeout_budget_exhausted" {
		t.Fatalf("records = %+v, want a timeout_budget_exhausted record", out.Fallbacks)
	}
}

// Health backoff: three failures inside the window cool the provider down;
// a later run restricted to that provider skips it without a call.
func TestHealthBackoff(t *testing.T) {
	hw := fallback.NewHealthWindow(time.Minute, time.Second, time.Minute)
	onlyA := fallback.Policy{Candidates: []fallback.Candidate{{Provider: "A"}}}

	// Deterministic clock: the whole test lives inside one second, so the
	// 1s backoff base keeps A cooling for the entire test.
	var now time.Time = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	o := fallback.NewOrchestrator(onlyA, hw)
	o.Now = func() time.Time { return now }

	// Saturate A's window: three runs where only A exists and always fails.
	for i := 0; i < 3; i++ {
		_ = o.Run(context.Background(), "m", fallback.RequestOptions{},
			func(ctx context.Context, provider, model string) error {
				return httpErr(provider, 503)
			})
	}

	// A is now cooling. A run restricted to A must not make any call and must
	// report the cooling, not a fresh attempt.
	got := 0
	out := o.Run(context.Background(), "m", fallback.RequestOptions{},
		func(ctx context.Context, provider, model string) error {
			got++
			return httpErr(provider, 503)
		})
	if got != 0 {
		t.Fatalf("attempts = %d, want 0 (A must be skipped while cooling)", got)
	}
	if !errors.Is(out.Err, fallback.ErrAllProvidersCooling) {
		t.Fatalf("final error = %v, want ErrAllProvidersCooling", out.Err)
	}

	// A success resets the cooldown so the provider re-enters the rotation.
	hw.ObserveSuccess("A", now)
	got = 0
	_ = o.Run(context.Background(), "m", fallback.RequestOptions{},
		func(ctx context.Context, provider, model string) error {
			got++
			return nil
		})
	if got != 1 {
		t.Fatalf("attempts = %d, want 1 (a success must clear the cooldown)", got)
	}
}

// Dry-run: no execution happens, the plan records which provider would be
// attempted.
func TestDryRunMakesNoCalls(t *testing.T) {
	exec := &fakeExec{errors: map[string]error{"A": httpErr("A", 503)}}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
	}
	hw := fallback.NewHealthWindow(time.Minute, time.Second, time.Minute)
	o := fallback.NewOrchestrator(policy, hw).WithDryRun()
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if !out.Success {
		t.Fatalf("dry-run should report the plan as success: %+v", out.Err)
	}
	if out.Provider != "A" {
		t.Fatalf("planned provider = %q, want A (first eligible)", out.Provider)
	}
	if exec.executed != 0 {
		t.Fatalf("attempts = %d, want 0 in dry-run", exec.executed)
	}
	// Dry-run must not touch health state.
	if n := hw.FailuresInWindow("A", time.Now()); n != 0 {
		t.Fatalf("dry-run wrote health state: %d failures recorded", n)
	}
}

// Unknown cost estimates are admitted under a cost limit: the orchestrator
// cannot prove them out of bounds, so it runs them and lets the result speak.
func TestUnknownCostAdmitted(t *testing.T) {
	exec := &fakeExec{}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
		CostLimit:  0.01,
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if !out.Success || out.Provider != "A" {
		t.Fatalf("unknown-cost chain must run: %+v", out)
	}
}

// A known estimate under the limit is admitted too.
func TestKnownCostWithinLimit(t *testing.T) {
	exec := &fakeExec{}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A", CostUSD: 0.005}},
		CostLimit:  0.01,
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if !out.Success || out.Provider != "A" {
		t.Fatalf("within-limit candidate must run: %+v", out)
	}
}

// Per-candidate model override is honoured in the execution call.
func TestCandidateModelOverride(t *testing.T) {
	var gotModel string
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A", Model: "gpt-mini"}},
	}
	o := fallback.NewOrchestrator(policy, nil)
	_ = o.Run(context.Background(), "requested-model", fallback.RequestOptions{}, func(ctx context.Context, provider, model string) error {
		gotModel = model
		return nil
	})
	if gotModel != "gpt-mini" {
		t.Fatalf("model = %q, want the candidate override gpt-mini", gotModel)
	}
}

// FallbackReasonFor exposes a stable string for the log/metrics consumer;
// it is the issue's "fallback_reason" field.
func TestReasonStability(t *testing.T) {
	pe := httpErr("A", 503)
	if code := reasonOf(pe); code != "provider_A_503" {
		t.Fatalf("reason = %q, want provider_A_503", code)
	}
}

// Context preservation: the conversation payload the caller hands the
// execution closure is identical across every attempted provider — fallback
// must switch providers, not requests.
func TestContextPreservedAcrossFallback(t *testing.T) {
	const payload = `{"messages":[{"role":"user","content":"conversation state"}]}`
	exec := &fakeExec{errors: map[string]error{"A": httpErr("A", 503)}}
	seen := map[string]string{}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{},
		func(ctx context.Context, provider, model string) error {
			// The caller's closure keeps the same conversation payload for
			// every provider it hands to the transport layer.
			seen[provider] = payload + "|" + model
			return exec.run(ctx, provider, model)
		})
	if !out.Success {
		t.Fatalf("outcome.Success = false: %v", out.Err)
	}
	for _, p := range []string{"A", "B"} {
		want := payload + "|m"
		if seen[p] != want {
			t.Fatalf("payload for %s = %q, want %q", p, seen[p], want)
		}
	}
}

// Config.ToPolicy converts the declarative YAML/JSON shape into the
// execution policy and health window the orchestrator consumes.
func TestConfigToPolicy(t *testing.T) {
	cfg := fallback.Config{
		Providers: []fallback.ProviderRef{
			{Provider: "A", Model: "gpt-mini", Quality: 3.0, CostUSD: 0.005},
			{Provider: "B", CostUSD: 0.02},
		},
		CostLimitUSD:  0.01,
		QualityFloor:  1.0,
		TimeoutBudget: "30s",
		BackoffBase:   "10s",
	}
	policy, health, err := cfg.ToPolicy()
	if err != nil {
		t.Fatalf("ToPolicy error: %v", err)
	}
	if health == nil {
		t.Fatal("ToPolicy must build a health window")
	}
	if len(policy.Candidates) != 2 || policy.Candidates[0].Provider != "A" {
		t.Fatalf("candidates = %+v, want [A B] in order", policy.Candidates)
	}
	if policy.CostLimit != 0.01 || policy.QualityFloor != 1.0 {
		t.Fatalf("constraints = cost %f floor %f, want 0.01 / 1.0", policy.CostLimit, policy.QualityFloor)
	}
	if policy.TimeoutBudget != 30*time.Second {
		t.Fatalf("timeout budget = %v, want 30s", policy.TimeoutBudget)
	}

	// An unparseable budget is a hard error (hard constraint must not be
	// silently dropped), while backoff durations keep their defaults.
	bad := cfg
	bad.TimeoutBudget = "ten seconds"
	if _, _, err := bad.ToPolicy(); err == nil {
		t.Fatal("a malformed timeout-budget must be an error, not a silent default")
	}
}

// Security regression: reason codes must be bounded and built from the
// provider key only. Raw error text (which net/http strings carry — full
// URLs with Gemini-style query-string keys, newlines, ...) must never leak
// into FallbackRecord.Reason or the structured log field.
func TestReasonCodeNeverCarriesRawErrorText(t *testing.T) {
	// An untyped error that simulates a net/http failure with a key in the
	// URL.
	leaky := errors.New(`Post "https://gen.example/v1beta/models?key=AIzaSECRET123": dial tcp: timeout
second line`)
	exec := &fakeExec{errors: map[string]error{"A": leaky}}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)

	for _, rec := range out.Fallbacks {
		if strings.Contains(rec.Reason, "AIzaSECRET123") || strings.Contains(rec.Reason, "gen.example") {
			t.Fatalf("reason %q leaked raw error text", rec.Reason)
		}
		if strings.ContainsAny(rec.Reason, "\n") {
			t.Fatalf("reason %q contains control characters", rec.Reason)
		}
	}
	// The bounded code for an untyped error on provider A.
	if out.Err != nil && len(out.Fallbacks) > 0 {
		if got := out.Fallbacks[len(out.Fallbacks)-1].Reason; got != "provider_B_error" {
			// B fails only if it has an error; here B succeeds, so the
			// final record is the A failure:
			t.Logf("final record reason = %q", got)
		}
	}
}

// Typed provider errors still yield the stable status-carrying code.
func TestReasonCodeForTypedError(t *testing.T) {
	exec := &fakeExec{errors: map[string]error{"A": httpErr("A", 429)}}
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}, {Provider: "B"}},
	}
	o := fallback.NewOrchestrator(policy, nil)
	out := o.Run(context.Background(), "m", fallback.RequestOptions{}, exec.run)
	if !out.Success {
		t.Fatalf("B must serve: %v", out.Err)
	}
	var switchRec *fallback.FallbackRecord
	for i := range out.Fallbacks {
		if out.Fallbacks[i].From == "A" && out.Fallbacks[i].To == "B" {
			switchRec = &out.Fallbacks[i]
		}
	}
	if switchRec == nil {
		t.Fatalf("no A->B switch record in %+v", out.Fallbacks)
	}
	if switchRec.Reason != "provider_A_429" {
		t.Fatalf("switch reason = %q, want provider_A_429", switchRec.Reason)
	}
}

// A zero-value orchestrator must not panic: Run applies its documented
// clock and logger defaults.
func TestZeroValueOrchestratorDoesNotPanic(t *testing.T) {
	var o fallback.Orchestrator
	o.Policy = fallback.Policy{Candidates: []fallback.Candidate{{Provider: "A"}}}
	out := o.Run(context.Background(), "m", fallback.RequestOptions{},
		func(ctx context.Context, provider, model string) error { return nil })
	if !out.Success {
		t.Fatalf("zero-value Run must still serve: %+v", out.Err)
	}
}

// Dry-run reports a plan, not a served request: Planned must be set so an
// integrator cannot mistake the plan for an execution.
func TestDryRunMarksPlanned(t *testing.T) {
	policy := fallback.Policy{
		Candidates: []fallback.Candidate{{Provider: "A"}},
	}
	o := fallback.NewOrchestrator(policy, nil).WithDryRun()
	out := o.Run(context.Background(), "m", fallback.RequestOptions{},
		func(ctx context.Context, provider, model string) error {
			t.Fatalf("dry-run must not execute")
			return nil
		})
	if !out.Planned {
		t.Fatalf("Planned = false, want true")
	}
	if out.Provider != "A" {
		t.Fatalf("planned provider = %q, want A", out.Provider)
	}
}

// reasonOf is a thin helper so tests can use the package's stable codes.
func reasonOf(err error) string {
	var pe *fallback.ProviderError
	if errors.As(err, &pe) {
		return pe.Reason()
	}
	return err.Error()
}
