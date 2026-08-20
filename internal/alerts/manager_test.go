package alerts

import (
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func usageRecord(target string, tokens int64, failed bool) coreusage.Record {
	return coreusage.Record{
		AuthIndex: target,
		Detail:    coreusage.Detail{TotalTokens: tokens},
		Failed:    failed,
	}
}

func TestManagerUsageLimitFires(t *testing.T) {
	manager := NewManager(Config{
		Enabled: true,
		Rules: []Rule{
			{Name: "limit", Kind: RuleUsageLimit, Severity: SeverityCritical, TokenLimit: 100},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 120, false))

	fired := manager.evaluateLocked()
	if len(fired) != 1 {
		t.Fatalf("fired = %d events, want 1", len(fired))
	}
	if fired[0].Rule != "limit" || fired[0].Target != "key-1" || fired[0].Tokens != 120 {
		t.Fatalf("unexpected event: %+v", fired[0])
	}
}

func TestManagerUsageLimitCooldown(t *testing.T) {
	manager := NewManager(Config{
		Enabled: true,
		Rules: []Rule{
			{Name: "limit", Kind: RuleUsageLimit, TokenLimit: 100, Cooldown: "1h"},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 120, false))

	if fired := manager.evaluateLocked(); len(fired) != 1 {
		t.Fatalf("first evaluation fired = %d events, want 1", len(fired))
	}
	if fired := manager.evaluateLocked(); len(fired) != 0 {
		t.Fatalf("second evaluation within cooldown fired = %d events, want 0", len(fired))
	}
}

func TestManagerCooldownExpires(t *testing.T) {
	manager := NewManager(Config{
		Enabled: true,
		Rules: []Rule{
			{Name: "limit", Kind: RuleUsageLimit, TokenLimit: 100, Cooldown: "1ms"},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 120, false))
	if fired := manager.evaluateLocked(); len(fired) != 1 {
		t.Fatalf("first evaluation fired = %d events, want 1", len(fired))
	}

	time.Sleep(5 * time.Millisecond)
	if fired := manager.evaluateLocked(); len(fired) != 1 {
		t.Fatalf("evaluation after cooldown fired = %d events, want 1", len(fired))
	}
}

func TestManagerDisabled(t *testing.T) {
	manager := NewManager(Config{
		Enabled: false,
		Rules: []Rule{
			{Name: "limit", Kind: RuleUsageLimit, TokenLimit: 100},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 120, false))
	if fired := manager.evaluateLocked(); len(fired) != 0 {
		t.Fatalf("disabled manager fired = %d events, want 0", len(fired))
	}
}

func TestManagerFaultRule(t *testing.T) {
	manager := NewManager(Config{
		Enabled: true,
		Rules: []Rule{
			{Name: "fault", Kind: RuleFault, ErrorCountLimit: 2},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 10, true))
	manager.HandleUsage(t.Context(), usageRecord("key-1", 10, true))

	fired := manager.evaluateLocked()
	if len(fired) != 1 || fired[0].Kind != RuleFault || fired[0].Errors != 2 {
		t.Fatalf("unexpected fault events: %+v", fired)
	}
}

func TestManagerAnomalyRule(t *testing.T) {
	manager := NewManager(Config{
		Enabled: true,
		Rules: []Rule{
			{Name: "anomaly", Kind: RuleAnomaly, ErrorRateLimit: 0.5},
		},
	})
	for i := 0; i < minRequestsForRate; i++ {
		failed := i%2 == 0
		manager.HandleUsage(t.Context(), usageRecord("key-1", 10, failed))
	}

	fired := manager.evaluateLocked()
	if len(fired) != 1 || fired[0].Kind != RuleAnomaly {
		t.Fatalf("unexpected anomaly events: %+v", fired)
	}
	if fired[0].ErrorRate < 0.5 {
		t.Fatalf("error rate = %f, want >= 0.5", fired[0].ErrorRate)
	}
}

func TestManagerTargetScoping(t *testing.T) {
	manager := NewManager(Config{
		Enabled: true,
		Rules: []Rule{
			{Name: "scoped", Kind: RuleUsageLimit, TokenLimit: 100, Target: "key-2"},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 120, false))
	manager.HandleUsage(t.Context(), usageRecord("key-2", 120, false))

	fired := manager.evaluateLocked()
	if len(fired) != 1 || fired[0].Target != "key-2" {
		t.Fatalf("unexpected scoped events: %+v", fired)
	}
}

func TestRuleValid(t *testing.T) {
	tests := []struct {
		rule Rule
		want bool
	}{
		{Rule{Name: "ok", Kind: RuleUsageLimit, TokenLimit: 1}, true},
		{Rule{Name: "ok", Kind: RuleAnomaly, ErrorRateLimit: 0.1}, true},
		{Rule{Name: "ok", Kind: RuleFault, ErrorCountLimit: 1}, true},
		{Rule{Name: "", Kind: RuleUsageLimit, TokenLimit: 1}, false},
		{Rule{Name: "bad-kind", Kind: RuleKind("nope")}, false},
		{Rule{Name: "no-threshold", Kind: RuleUsageLimit}, false},
		{Rule{Name: "bad-rate", Kind: RuleAnomaly, ErrorRateLimit: 0}, false},
	}
	for _, tt := range tests {
		if got := tt.rule.valid(); got != tt.want {
			t.Errorf("Rule%+v.valid() = %v, want %v", tt.rule, got, tt.want)
		}
	}
}

func TestManagerSnapshot(t *testing.T) {
	manager := NewManager(Config{
		Enabled:          true,
		CheckInterval:    "30s",
		FeishuWebhookURL: "https://example.com/feishu",
		Rules: []Rule{
			{Name: "limit", Kind: RuleUsageLimit, TokenLimit: 100},
		},
	})
	manager.HandleUsage(t.Context(), usageRecord("key-1", 120, false))
	manager.evaluateLocked()

	snapshot := manager.Snapshot()
	if !snapshot.Enabled {
		t.Fatal("snapshot.Enabled = false, want true")
	}
	if snapshot.CheckInterval != "30s" {
		t.Fatalf("snapshot.CheckInterval = %q, want 30s", snapshot.CheckInterval)
	}
	if len(snapshot.Channels) != 1 || snapshot.Channels[0] != ChannelFeishu {
		t.Fatalf("snapshot.Channels = %v, want [feishu]", snapshot.Channels)
	}
	if len(snapshot.Events) != 1 {
		t.Fatalf("snapshot.Events = %d, want 1", len(snapshot.Events))
	}
}
