package alerts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDingTalkSignature(t *testing.T) {
	got := DingTalkSignature("SEC", 1234567890)
	want := "+yDy0bk2vrKfWbG6ykPZ2zxbV7O6EVvVzoIa4jjxGwI="
	if got != want {
		t.Fatalf("DingTalkSignature() = %q, want %q", got, want)
	}
	if _, err := base64.StdEncoding.DecodeString(got); err != nil {
		t.Fatalf("DingTalkSignature() is not valid base64: %v", err)
	}
}

func TestSignDingTalkURLAppendsSignedQuery(t *testing.T) {
	signed := signDingTalkURL("https://oapi.dingtalk.com/robot/send?access_token=abc", "SEC")
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("signed URL does not parse: %v", err)
	}
	if parsed.Query().Get("access_token") != "abc" {
		t.Fatalf("original access_token lost: %v", parsed.Query())
	}
	if parsed.Query().Get("timestamp") == "" {
		t.Fatalf("timestamp query param missing: %s", signed)
	}
	sign := parsed.Query().Get("sign")
	if sign == "" {
		t.Fatalf("sign query param missing: %s", signed)
	}
	if _, err := base64.StdEncoding.DecodeString(sign); err != nil {
		t.Fatalf("sign query param is not base64: %v", err)
	}
}

func TestWebhookNotifierSendText(t *testing.T) {
	tests := []struct {
		name        string
		kind        ChannelKind
		wantMsgKey  string
		wantContent any
	}{
		{
			name:       "feishu",
			kind:       ChannelFeishu,
			wantMsgKey: "msg_type",
			wantContent: map[string]any{
				"msg_type": "text",
				"content":  map[string]any{"text": "hello"},
			},
		},
		{
			name:       "dingtalk",
			kind:       ChannelDingTalk,
			wantMsgKey: "msgtype",
			wantContent: map[string]any{
				"msgtype": "text",
				"text":    map[string]any{"content": "hello"},
			},
		},
		{
			name:       "wecom",
			kind:       ChannelWeCom,
			wantMsgKey: "msgtype",
			wantContent: map[string]any{
				"msgtype": "text",
				"text":    map[string]any{"content": "hello"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				if err := json.Unmarshal(body, &captured); err != nil {
					t.Errorf("unmarshal body: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			notifier := newWebhookNotifier(tt.kind, server.URL, "", server.Client())
			if err := notifier.SendText(context.Background(), "hello"); err != nil {
				t.Fatalf("SendText() error = %v", err)
			}
			if captured == nil {
				t.Fatal("no payload captured")
			}
			if _, ok := captured[tt.wantMsgKey]; !ok {
				t.Fatalf("payload missing %q: %v", tt.wantMsgKey, captured)
			}
		})
	}
}

func TestWebhookNotifierSendTextHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notifier := newWebhookNotifier(ChannelFeishu, server.URL, "", server.Client())
	if err := notifier.SendText(context.Background(), "hello"); err == nil {
		t.Fatal("SendText() expected error for 500 response, got nil")
	}
}

func TestDispatcherSendTextUnknownChannel(t *testing.T) {
	dispatcher := newDispatcher(Config{})
	if err := dispatcher.SendText(context.Background(), "telegram", "hello"); err == nil {
		t.Fatal("SendText() expected error for unknown channel, got nil")
	}
}

func TestFormatEvent(t *testing.T) {
	event := Event{
		Severity: SeverityCritical,
		Kind:     RuleFault,
		Target:   "key-1",
		Message:  "fault detected",
	}
	got := formatEvent(event)
	if !strings.Contains(got, "[critical]") || !strings.Contains(got, "fault detected") {
		t.Fatalf("formatEvent() = %q", got)
	}
}
