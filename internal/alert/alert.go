// Package alert provides enterprise multi-channel alert notifications for
// Feishu (飞书), DingTalk (钉钉), and WeCom (企业微信) webhook robots.
// It supports severity levels, template rendering, silence dedup, and
// automatic recovery notifications.
package alert

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Severity represents the alert severity level.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityRecovery Severity = "recovery"
)

// Channel identifies the IM channel type.
type Channel string

const (
	ChannelFeishu   Channel = "feishu"
	ChannelDingTalk Channel = "dingtalk"
	ChannelWeCom    Channel = "wecom"
)

// AlertMessage is the payload sent to webhook channels.
type AlertMessage struct {
	// ID uniquely identifies an alert for deduplication.
	ID string
	// Severity of the alert.
	Severity Severity
	// Title is a short human-readable alert title.
	Title string
	// Body is the detailed description.
	Body string
	// ServiceName identifies the affected service.
	ServiceName string
	// ErrorRate is the current error rate (0.0–1.0).
	ErrorRate float64
	// AffectedProviders lists the impacted upstream providers.
	AffectedProviders []string
	// Latency is the observed response latency.
	Latency time.Duration
	// Timestamp is when the alert was generated.
	Timestamp time.Time
}

// ChannelConfig holds webhook endpoint configuration for a single channel.
type ChannelConfig struct {
	// WebhookURL is the incoming robot webhook URL.
	WebhookURL string `yaml:"webhook-url" json:"webhook-url"`
	// SignSecret is the HMAC-SHA256 signing secret (optional per channel).
	SignSecret string `yaml:"sign-secret,omitempty" json:"sign-secret,omitempty"`
	// Enabled controls whether this channel is active.
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// Config holds the complete alert notification configuration.
type Config struct {
	// Channels maps channel name to its webhook configuration.
	Channels map[Channel]ChannelConfig `yaml:"channels" json:"channels"`
	// SilencePeriod prevents duplicate alerts within this duration.
	SilencePeriod time.Duration `yaml:"silence-period" json:"silence-period"`
	// DefaultSilencePeriod is used when SilencePeriod is zero.
	DefaultSilencePeriod time.Duration `yaml:"-"`
}

// Notifier handles alert sending with dedup, silence, and recovery logic.
type Notifier struct {
	mu         sync.Mutex
	config     Config
	senders    map[Channel]Sender
	silenced   map[string]time.Time    // alert key -> expiry
	lastAlert  map[string]AlertMessage // dedup key -> last sent alert
	httpClient HTTPClient
}

// Sender sends an alert message to a specific IM channel.
type Sender interface {
	// Channel returns the channel type.
	Channel() Channel
	// Send delivers the alert message to the webhook. Returns an error if delivery fails.
	Send(ctx context.Context, msg AlertMessage) error
}

// HTTPClient is the interface for sending HTTP requests.
type HTTPClient interface {
	PostJSON(ctx context.Context, url string, body interface{}) ([]byte, error)
}

// NewNotifier creates a new Notifier with the given configuration and HTTP client.
func NewNotifier(cfg Config, client HTTPClient) *Notifier {
	if client == nil {
		client = defaultHTTPClient
	}
	if cfg.DefaultSilencePeriod == 0 {
		cfg.DefaultSilencePeriod = 5 * time.Minute
	}

	n := &Notifier{
		config:     cfg,
		senders:    make(map[Channel]Sender),
		silenced:   make(map[string]time.Time),
		lastAlert:  make(map[string]AlertMessage),
		httpClient: client,
	}

	// Register senders for enabled channels.
	for ch, cc := range cfg.Channels {
		if !cc.Enabled || cc.WebhookURL == "" {
			continue
		}
		var s Sender
		switch ch {
		case ChannelFeishu:
			s = NewFeishuSender(cc, client)
		case ChannelDingTalk:
			s = NewDingTalkSender(cc, client)
		case ChannelWeCom:
			s = NewWeComSender(cc, client)
		default:
			log.Warnf("alert: unknown channel %q, skipping", ch)
			continue
		}
		n.senders[ch] = s
	}

	return n
}

// Send dispatches an alert to all enabled channels, respecting silence periods
// and deduplication. Returns the number of channels successfully notified.
func (n *Notifier) Send(ctx context.Context, msg AlertMessage) int {
	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	dedupKey := n.dedupKey(msg)
	silenceDuration := n.config.SilencePeriod
	if silenceDuration <= 0 {
		silenceDuration = n.config.DefaultSilencePeriod
	}

	n.mu.Lock()
	// Check silence window.
	if expiry, ok := n.silenced[dedupKey]; ok && time.Now().Before(expiry) {
		// If this is a recovery, still send it (recovery should always go through).
		if msg.Severity != SeverityRecovery {
			n.mu.Unlock()
			log.Debugf("alert: silenced %s (dedup=%s), skipping", msg.ID, dedupKey)
			return 0
		}
	}
	// Check dedup — skip if same message sent within silence period.
	if last, ok := n.lastAlert[dedupKey]; ok {
		if last.Severity == msg.Severity &&
			last.ErrorRate == msg.ErrorRate &&
			time.Since(last.Timestamp) < silenceDuration {
			log.Debugf("alert: dedup %s, skipping", msg.ID)
			n.mu.Unlock()
			return 0
		}
	}

	// Set silence period.
	n.silenced[dedupKey] = time.Now().Add(silenceDuration)
	n.lastAlert[dedupKey] = msg
	n.mu.Unlock()

	// Send to all enabled channels.
	var sent int
	for ch, s := range n.senders {
		if err := s.Send(ctx, msg); err != nil {
			log.Errorf("alert: failed to send to %s: %v", ch, err)
			continue
		}
		sent++
	}
	return sent
}

// SendRecovery sends a recovery notification if a previous alert was sent for the key.
// This is a convenience method that sends a SeverityRecovery message.
func (n *Notifier) SendRecovery(ctx context.Context, serviceName string, msg AlertMessage) {
	msg.Severity = SeverityRecovery
	if msg.Title == "" {
		msg.Title = fmt.Sprintf("[恢复] %s 已恢复正常", serviceName)
	}
	if msg.Body == "" {
		msg.Body = fmt.Sprintf("服务 %s 已恢复正常运行", serviceName)
	}
	msg.ServiceName = serviceName
	n.Send(ctx, msg)
}

func (n *Notifier) dedupKey(msg AlertMessage) string {
	return fmt.Sprintf("%s/%s", msg.ServiceName, msg.Title)
}

// Senders returns the registered senders for inspection.
func (n *Notifier) Senders() map[Channel]Sender {
	return n.senders
}
