package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Notifier pushes alert text to a single platform.
type Notifier interface {
	SendText(ctx context.Context, text string) error
}

// dispatcher fans alerts out to every configured channel and exposes a
// per-channel test send path.
type dispatcher struct {
	channels []ChannelKind
	byKind   map[ChannelKind]Notifier
}

func newDispatcher(cfg Config) *dispatcher {
	dispatcher := &dispatcher{byKind: make(map[ChannelKind]Notifier)}
	client := &http.Client{Timeout: cfg.Timeout()}

	add := func(kind ChannelKind, webhook, secret string) {
		if strings.TrimSpace(webhook) == "" {
			return
		}
		dispatcher.channels = append(dispatcher.channels, kind)
		dispatcher.byKind[kind] = newWebhookNotifier(kind, strings.TrimSpace(webhook), secret, client)
	}
	add(ChannelFeishu, cfg.FeishuWebhookURL, "")
	add(ChannelDingTalk, cfg.DingTalkWebhookURL, cfg.DingTalkSecret)
	add(ChannelWeCom, cfg.WeComWebhookURL, "")
	return dispatcher
}

// EnabledChannels returns the configured channels in registration order.
func (d *dispatcher) EnabledChannels() []ChannelKind {
	if d == nil {
		return nil
	}
	channels := make([]ChannelKind, len(d.channels))
	copy(channels, d.channels)
	return channels
}

// Send pushes the event text to every configured channel. Delivery is
// best-effort: each notifier logs its own failures without failing the others.
func (d *dispatcher) Send(ctx context.Context, event Event) {
	if d == nil {
		return
	}
	text := formatEvent(event)
	for _, kind := range d.channels {
		if notifier := d.byKind[kind]; notifier != nil {
			if err := notifier.SendText(ctx, text); err != nil {
				log.WithError(err).Warnf("alerts: %s notification failed", kind)
			}
		}
	}
}

// SendText sends a raw message to a single configured channel. It is used by
// the management API test endpoint.
func (d *dispatcher) SendText(ctx context.Context, kind ChannelKind, text string) error {
	if d == nil || !kind.Valid() {
		return fmt.Errorf("alerts: unknown channel %q", kind)
	}
	notifier := d.byKind[kind]
	if notifier == nil {
		return fmt.Errorf("alerts: channel %q is not configured", kind)
	}
	return notifier.SendText(ctx, text)
}

func formatEvent(event Event) string {
	severity := event.Severity
	if severity == "" {
		severity = SeverityInfo
	}
	message := strings.TrimSpace(event.Message)
	if message == "" {
		message = fmt.Sprintf("%s alert for %s", event.Kind, event.Target)
	}
	return fmt.Sprintf("[%s] %s", severity, message)
}

// webhookNotifier pushes text messages to a bot webhook.
type webhookNotifier struct {
	kind       ChannelKind
	webhookURL string
	secret     string
	client     *http.Client
}

func newWebhookNotifier(kind ChannelKind, webhookURL, secret string, client *http.Client) *webhookNotifier {
	return &webhookNotifier{kind: kind, webhookURL: webhookURL, secret: secret, client: client}
}

// SendText builds the platform-specific payload and posts it to the webhook.
func (n *webhookNotifier) SendText(ctx context.Context, text string) error {
	if n == nil || n.client == nil {
		return fmt.Errorf("alerts: %s notifier unavailable", n.kind)
	}

	payload := map[string]any{}
	switch n.kind {
	case ChannelFeishu:
		payload["msg_type"] = "text"
		payload["content"] = map[string]any{"text": text}
	case ChannelDingTalk, ChannelWeCom:
		payload["msgtype"] = "text"
		payload["text"] = map[string]any{"content": text}
	default:
		return fmt.Errorf("alerts: unsupported channel %q", n.kind)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("alerts: marshal %s payload: %w", n.kind, err)
	}

	target := n.webhookURL
	if n.kind == ChannelDingTalk && n.secret != "" {
		target = signDingTalkURL(n.webhookURL, n.secret)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alerts: build %s request: %w", n.kind, err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := n.client.Do(request)
	if err != nil {
		return fmt.Errorf("alerts: send %s notification: %w", n.kind, err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Warnf("alerts: close %s response body", n.kind)
		}
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("alerts: %s webhook returned %s", n.kind, response.Status)
	}
	return nil
}

// DingTalkSignature computes the HMAC-SHA256 signature DingTalk expects when a
// bot has signing enabled. The result is base64-encoded; callers URL-encode it
// when appending it to the webhook URL.
func DingTalkSignature(secret string, timestamp int64) string {
	stringToSign := strconv.FormatInt(timestamp, 10) + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func signDingTalkURL(webhookURL, secret string) string {
	timestamp := time.Now().UnixMilli()
	signature := DingTalkSignature(secret, timestamp)
	separator := "&"
	if !strings.Contains(webhookURL, "?") {
		separator = "?"
	}
	return webhookURL + separator + "timestamp=" + strconv.FormatInt(timestamp, 10) + "&sign=" + url.QueryEscape(signature)
}
