package contentfilter

import (
	"os"
	"strings"
	"testing"
)

// TestLiveKeeperIntegration verifies the syncer can pull rules from the real
// KEEPER container and the engine masks real content. It is opt-in because it
// requires Docker + the KEEPER container; run with:
//
//	CPA_CONTENT_FILTER_LIVE_TEST=1 go test ./internal/contentfilter/ -run TestLiveKeeperIntegration -v
func TestLiveKeeperIntegration(t *testing.T) {
	if os.Getenv("CPA_CONTENT_FILTER_LIVE_TEST") != "1" {
		t.Skip("live KEEPER integration test skipped (set CPA_CONTENT_FILTER_LIVE_TEST=1)")
	}

	opts := DefaultSyncerOptions()
	// Force docker-cp mode by clearing the host path (not present on macOS).
	opts.HostDBPath = ""
	opts.ContainerName = "enterprise-keeper-cpa-usage-keeper-1"
	opts.ContainerDBPath = "/data/app.db"

	s, err := NewSyncer(opts)
	if err != nil {
		t.Fatalf("NewSyncer against live KEEPER: %v", err)
	}
	s.Start()
	defer s.Stop()

	rules := s.Rules()
	if len(rules) < 3 {
		t.Fatalf("live KEEPER rules = %d, want >= 3", len(rules))
	}
	t.Logf("loaded %d live rules:", len(rules))
	for _, r := range rules {
		t.Logf("  id=%d name=%q scenario=%s words=%v pii=%v",
			r.ID, r.Name, r.Scenario, r.SensitiveWords, r.PIITypes)
	}

	e := NewEngine(true)

	// Acceptance #3: inbound sensitive word "绝密文件" -> replaced with mask.
	res := e.Apply(rules, "该文件属于绝密文件，请勿外传", true, "")
	if !res.Changed || strings.Contains(res.Text, "绝密文件") {
		t.Fatalf("inbound sensitive word not masked: %q", res.Text)
	}
	t.Logf("inbound sensitive word -> %q", res.Text)

	// Acceptance #4: outbound PII phone partial masked (138****1234 style).
	phoneRes := e.Apply(rules, "联系电话：13800138000，请查收", false, "")
	if !phoneRes.Changed {
		t.Fatalf("outbound phone not masked: %q", phoneRes.Text)
	}
	t.Logf("outbound phone -> %q", phoneRes.Text)

	// Acceptance #4b: ID card partial masked.
	idRes := e.Apply(rules, "身份证 11010519491231002X 已登记", false, "")
	if !idRes.Changed {
		t.Fatalf("outbound id card not masked: %q", idRes.Text)
	}
	t.Logf("outbound id card -> %q", idRes.Text)

	// Stale flag must be false after a successful load.
	if s.Stale() {
		t.Fatalf("syncer marked stale after successful load")
	}
}
