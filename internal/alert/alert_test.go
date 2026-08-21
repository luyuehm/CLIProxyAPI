package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockHTTPClient records requests and returns canned responses.
type mockHTTPClient struct {
	mu         sync.Mutex
	requests   []requestRecord
	responseFn func(url string, body interface{}) ([]byte, error)
}

type requestRecord struct {
	URL  string
	Body interface{}
}

func (m *mockHTTPClient) PostJSON(_ context.Context, url string, body interface{}) ([]byte, error) {
	m.mu.Lock()
	m.requests = append(m.requests, requestRecord{URL: url, Body: body})
	fn := m.responseFn
	m.mu.Unlock()
	if fn != nil {
		return fn(url, body)
	}
	return []byte(`{"ok":true}`), nil
}

func (m *mockHTTPClient) Requests() []requestRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := make([]requestRecord, len(m.requests))
	copy(r, m.requests)
	return r
}

// newTestNotifier creates a Notifier with all channels enabled for testing.
func newTestNotifier(client HTTPClient, silencePeriod time.Duration) *Notifier {
	if client == nil {
		client = &mockHTTPClient{}
	}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelFeishu:   {WebhookURL: "https://feishu.test/hook", Enabled: true},
			ChannelDingTalk: {WebhookURL: "https://dingtalk.test/robot", SignSecret: "test-secret", Enabled: true},
			ChannelWeCom:    {WebhookURL: "https://wecom.test/webhook", Enabled: true},
		},
		SilencePeriod:        silencePeriod,
		DefaultSilencePeriod: 5 * time.Minute,
	}
	return NewNotifier(cfg, client)
}

// assertStringsContains checks that s contains substr.
func assertStringsContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}

func TestFeishuSender_PayloadFormat(t *testing.T) {
	mock := &mockHTTPClient{}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelFeishu: {WebhookURL: "https://feishu.test/hook", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:                "test-1",
		Severity:          SeverityCritical,
		Title:             "服务异常",
		ServiceName:       "gateway",
		ErrorRate:         0.15,
		AffectedProviders: []string{"claude", "openai"},
		Latency:           2500 * time.Millisecond,
		Timestamp:         time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC),
	}
	n.Send(context.Background(), msg)

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	body, err := json.Marshal(reqs[0].Body)
	if err != nil {
		t.Fatal(err)
	}

	var card struct {
		MsgType string `json:"msg_type"`
		Card    struct {
			Header struct {
				Title struct {
					Content string `json:"content"`
				} `json:"title"`
			} `json:"header"`
		} `json:"card"`
	}
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	if card.MsgType != "interactive" {
		t.Errorf("expected msg_type interactive, got %s", card.MsgType)
	}
	if card.Card.Header.Title.Content != "服务异常" {
		t.Errorf("expected title '服务异常', got %s", card.Card.Header.Title.Content)
	}
	if reqs[0].URL != "https://feishu.test/hook" {
		t.Errorf("expected feishu URL, got %s", reqs[0].URL)
	}
}

func TestDingTalkSender_PayloadFormat(t *testing.T) {
	var capturedURL string
	mock := &mockHTTPClient{
		responseFn: func(url string, body interface{}) ([]byte, error) {
			capturedURL = url
			return []byte(`{"errcode":0}`), nil
		},
	}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelDingTalk: {WebhookURL: "https://dingtalk.test/robot", SignSecret: "ding-secret", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:                "test-2",
		Severity:          SeverityWarning,
		Title:             "Provider 限流",
		ServiceName:       "claude-provider",
		ErrorRate:         0.08,
		AffectedProviders: []string{"claude-1"},
		Latency:           5000 * time.Millisecond,
		Timestamp:         time.Date(2026, 8, 22, 10, 5, 0, 0, time.UTC),
	}
	n.Send(context.Background(), msg)

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	if !strings.Contains(capturedURL, "timestamp=") {
		t.Error("expected signed URL to contain timestamp")
	}
	if !strings.Contains(capturedURL, "sign=") {
		t.Error("expected signed URL to contain sign")
	}

	body, err := json.Marshal(reqs[0].Body)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		MsgType    string `json:"msgtype"`
		ActionCard *struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"actionCard"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.MsgType != "actionCard" {
		t.Errorf("expected actionCard, got %s", payload.MsgType)
	}
	if payload.ActionCard == nil {
		t.Fatal("expected actionCard to be non-nil")
	}
	if payload.ActionCard.Title != "Provider 限流" {
		t.Errorf("expected title 'Provider 限流', got %s", payload.ActionCard.Title)
	}
	if !strings.Contains(payload.ActionCard.Text, "8.0%") {
		t.Error("expected text to contain error rate 8.0%")
	}
}

func TestWeComSender_PayloadFormat(t *testing.T) {
	mock := &mockHTTPClient{}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelWeCom: {WebhookURL: "https://wecom.test/webhook", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:                "test-3",
		Severity:          SeverityWarning,
		Title:             "API Latency Spike",
		ServiceName:       "api-gateway",
		ErrorRate:         0.22,
		AffectedProviders: []string{"provider-a"},
		Latency:           3000 * time.Millisecond,
		Timestamp:         time.Date(2026, 8, 22, 10, 10, 0, 0, time.UTC),
	}
	n.Send(context.Background(), msg)

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	body, err := json.Marshal(reqs[0].Body)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		MsgType  string `json:"msgtype"`
		Markdown *struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.MsgType != "markdown" {
		t.Errorf("expected markdown msgtype, got %s", payload.MsgType)
	}
	if payload.Markdown == nil {
		t.Fatal("expected markdown to be non-nil")
	}
	assertStringsContains(t, payload.Markdown.Content, "22.0%")
	assertStringsContains(t, payload.Markdown.Content, "api-gateway")
}

func TestNotifier_SilenceDedup(t *testing.T) {
	mock := &mockHTTPClient{}
	n := newTestNotifier(mock, 10*time.Minute)

	msg := AlertMessage{
		ID:          "dedup-1",
		Severity:    SeverityCritical,
		Title:       "Repeated Alert",
		ServiceName: "test-service",
		ErrorRate:   0.5,
		Timestamp:   time.Now(),
	}

	sent1 := n.Send(context.Background(), msg)
	if sent1 != 3 {
		t.Errorf("expected 3 channels sent, got %d", sent1)
	}

	sent2 := n.Send(context.Background(), msg)
	if sent2 != 0 {
		t.Errorf("expected duplicate to be silenced (0), got %d", sent2)
	}

	// Different severity but same dedup key.
	msg2 := msg
	msg2.Severity = SeverityWarning
	sent3 := n.Send(context.Background(), msg2)
	if sent3 != 0 {
		t.Errorf("expected same-key warning to be silenced (0), got %d", sent3)
	}

	// Recovery should bypass silence.
	msg3 := msg
	msg3.Severity = SeverityRecovery
	sent4 := n.Send(context.Background(), msg3)
	if sent4 != 3 {
		t.Errorf("expected recovery to bypass silence (3), got %d", sent4)
	}
}

func TestNotifier_DedupDifferentService(t *testing.T) {
	mock := &mockHTTPClient{}
	n := newTestNotifier(mock, 10*time.Minute)

	msg1 := AlertMessage{
		ID:          "dedup-a",
		Severity:    SeverityCritical,
		Title:       "Outage",
		ServiceName: "service-a",
		Timestamp:   time.Now(),
	}
	msg2 := AlertMessage{
		ID:          "dedup-b",
		Severity:    SeverityCritical,
		Title:       "Outage",
		ServiceName: "service-b",
		Timestamp:   time.Now(),
	}

	sent1 := n.Send(context.Background(), msg1)
	sent2 := n.Send(context.Background(), msg2)
	if sent1 != 3 {
		t.Errorf("service-a: expected 3, got %d", sent1)
	}
	if sent2 != 3 {
		t.Errorf("service-b (different service): expected 3, got %d", sent2)
	}
}

func TestNotifier_PartialChannels(t *testing.T) {
	mock := &mockHTTPClient{}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelFeishu: {WebhookURL: "https://feishu.test/hook", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:          "partial-1",
		Severity:    SeverityCritical,
		Title:       "Partial Channel Test",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}
	sent := n.Send(context.Background(), msg)
	if sent != 1 {
		t.Errorf("expected 1 (only Feishu), got %d", sent)
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if reqs[0].URL != "https://feishu.test/hook" {
		t.Errorf("expected feishu URL, got %s", reqs[0].URL)
	}
}

func TestNotifier_NoEnabledChannels(t *testing.T) {
	mock := &mockHTTPClient{}
	cfg := Config{
		Channels:             map[Channel]ChannelConfig{},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:          "nochan-1",
		Severity:    SeverityWarning,
		Title:       "No Channels",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}
	sent := n.Send(context.Background(), msg)
	if sent != 0 {
		t.Errorf("expected 0 (no channels), got %d", sent)
	}
}

func TestRecoveryNotification(t *testing.T) {
	mock := &mockHTTPClient{}
	n := newTestNotifier(mock, 5*time.Minute)

	alertMsg := AlertMessage{
		ID:          "recovery-1",
		Severity:    SeverityCritical,
		Title:       "Service Down",
		ServiceName: "gateway",
		ErrorRate:   0.95,
		Timestamp:   time.Now(),
	}
	sent1 := n.Send(context.Background(), alertMsg)
	if sent1 != 3 {
		t.Errorf("expected 3, got %d", sent1)
	}

	n.SendRecovery(context.Background(), "gateway", AlertMessage{
		ID: "recovery-2",
	})
}

func TestBuildBody(t *testing.T) {
	msg := AlertMessage{
		ServiceName:       "api-gateway",
		Severity:          SeverityCritical,
		ErrorRate:         0.123,
		Latency:           1500 * time.Millisecond,
		AffectedProviders: []string{"claude", "gemini"},
		Timestamp:         time.Date(2026, 8, 22, 12, 30, 0, 0, time.UTC),
	}
	body := buildBody(msg)
	assertStringsContains(t, body, "api-gateway")
	assertStringsContains(t, body, "critical")
	assertStringsContains(t, body, "12.3%")
	assertStringsContains(t, body, "claude")
	assertStringsContains(t, body, "gemini")
}

func TestSignV1(t *testing.T) {
	sign := signV1("test-secret", 1724320800000)
	if sign == "" {
		t.Fatal("expected non-empty signature")
	}

	sign2 := signV1("test-secret", 1724320800000)
	if sign != sign2 {
		t.Error("same inputs should produce same output")
	}

	sign3 := signV1("other-secret", 1724320800000)
	if sign == sign3 {
		t.Error("different secret should produce different output")
	}
}

func TestHTTPClient_Timeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer slow.Close()

	client := NewHTTPClient(50 * time.Millisecond)
	ctx := context.Background()
	_, err := client.PostJSON(ctx, slow.URL, map[string]string{"test": "data"})
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestHTTPClient_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("expected json content-type, got %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "test-data") {
			t.Error("body should contain test-data")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	ctx := context.Background()
	resp, err := client.PostJSON(ctx, srv.URL, map[string]string{"key": "test-data"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), `"ok":true`) {
		t.Errorf("unexpected response: %s", string(resp))
	}
}

func TestHTTPClient_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errcode":40001,"errmsg":"invalid webhook"}`)
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	ctx := context.Background()
	_, err := client.PostJSON(ctx, srv.URL, map[string]string{"test": "data"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 in error, got %s", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid webhook") {
		t.Errorf("expected invalid webhook in error, got %s", err.Error())
	}
}

func TestNotifier_DingTalkWithoutSignSecret(t *testing.T) {
	var capturedURL string
	mock := &mockHTTPClient{
		responseFn: func(url string, body interface{}) ([]byte, error) {
			capturedURL = url
			return []byte(`{"errcode":0}`), nil
		},
	}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelDingTalk: {WebhookURL: "https://dingtalk.test/robot", SignSecret: "", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:          "no-sign-1",
		Severity:    SeverityWarning,
		Title:       "No Sign",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}
	n.Send(context.Background(), msg)

	if capturedURL != "https://dingtalk.test/robot" {
		t.Errorf("expected unchanged URL, got %s", capturedURL)
	}
}

func TestNotifier_ConcurrentSafety(t *testing.T) {
	mock := &mockHTTPClient{}
	n := newTestNotifier(mock, 5*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := AlertMessage{
				ID:          fmt.Sprintf("concurrent-%d", i),
				Severity:    SeverityCritical,
				Title:       fmt.Sprintf("Alert %d", i),
				ServiceName: fmt.Sprintf("service-%d", i%3),
				Timestamp:   time.Now(),
			}
			n.Send(context.Background(), msg)
		}(i)
	}
	wg.Wait()

	reqs := mock.Requests()
	if len(reqs) < 3 {
		t.Errorf("expected at least 3 requests (3 distinct services), got %d", len(reqs))
	}
	if len(reqs) > 30 {
		t.Errorf("expected at most 30 requests (10 sends × 3 channels), got %d", len(reqs))
	}
}

func TestNotifier_SilenceExpiry(t *testing.T) {
	mock := &mockHTTPClient{}
	n := newTestNotifier(mock, 50*time.Millisecond)

	msg := AlertMessage{
		ID:          "expiry-1",
		Severity:    SeverityCritical,
		Title:       "Ephemeral Alert",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}

	sent1 := n.Send(context.Background(), msg)
	if sent1 != 3 {
		t.Errorf("expected 3, got %d", sent1)
	}

	time.Sleep(100 * time.Millisecond)

	sent2 := n.Send(context.Background(), msg)
	if sent2 != 3 {
		t.Errorf("expected alert to be re-sent after silence expiry (3), got %d", sent2)
	}
}

func TestNotifier_EmptyWebhookURL(t *testing.T) {
	mock := &mockHTTPClient{}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelFeishu: {WebhookURL: "", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:          "empty-url-1",
		Severity:    SeverityCritical,
		Title:       "Empty URL",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}
	sent := n.Send(context.Background(), msg)
	if sent != 0 {
		t.Errorf("expected 0 (no sender registered for empty URL), got %d", sent)
	}
}

func TestSeverityConstants(t *testing.T) {
	if string(SeverityCritical) != "critical" {
		t.Errorf("unexpected critical: %s", SeverityCritical)
	}
	if string(SeverityWarning) != "warning" {
		t.Errorf("unexpected warning: %s", SeverityWarning)
	}
	if string(SeverityRecovery) != "recovery" {
		t.Errorf("unexpected recovery: %s", SeverityRecovery)
	}
}

func TestChannelConstants(t *testing.T) {
	if string(ChannelFeishu) != "feishu" {
		t.Errorf("unexpected feishu: %s", ChannelFeishu)
	}
	if string(ChannelDingTalk) != "dingtalk" {
		t.Errorf("unexpected dingtalk: %s", ChannelDingTalk)
	}
	if string(ChannelWeCom) != "wecom" {
		t.Errorf("unexpected wecom: %s", ChannelWeCom)
	}
}

func TestHTTPClient_MaxResponseBody(t *testing.T) {
	// Return a large response body that should be truncated.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":"`+strings.Repeat("x", 70*1024)+`"}`)
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	ctx := context.Background()
	resp, err := client.PostJSON(ctx, srv.URL, map[string]string{"test": "data"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp) > 66*1024 {
		t.Errorf("expected truncated response <= 66KB, got %d bytes", len(resp))
	}
}

func TestNotifier_SendersDisableOneChannel(t *testing.T) {
	mock := &mockHTTPClient{}
	cfg := Config{
		Channels: map[Channel]ChannelConfig{
			ChannelFeishu:   {WebhookURL: "https://feishu.test/hook", Enabled: true},
			ChannelDingTalk: {WebhookURL: "https://dingtalk.test/robot", Enabled: false},
			ChannelWeCom:    {WebhookURL: "https://wecom.test/webhook", Enabled: true},
		},
		DefaultSilencePeriod: 5 * time.Minute,
	}
	n := NewNotifier(cfg, mock)

	msg := AlertMessage{
		ID:          "disable-one-1",
		Severity:    SeverityCritical,
		Title:       "Partial Test",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}
	sent := n.Send(context.Background(), msg)
	if sent != 2 {
		t.Errorf("expected 2 (feishu + wecom), got %d", sent)
	}
}

func TestNotifier_DefaultSilencePeriod(t *testing.T) {
	mock := &mockHTTPClient{}
	n := newTestNotifier(mock, 0)

	// With zero silence period, the default should apply.
	msg := AlertMessage{
		ID:          "default-silence-1",
		Severity:    SeverityCritical,
		Title:       "Default Silence",
		ServiceName: "test",
		Timestamp:   time.Now(),
	}
	sent1 := n.Send(context.Background(), msg)
	if sent1 != 3 {
		t.Errorf("expected 3, got %d", sent1)
	}
	sent2 := n.Send(context.Background(), msg)
	if sent2 != 0 {
		t.Errorf("expected duplicate to be silenced (0), got %d", sent2)
	}
}
