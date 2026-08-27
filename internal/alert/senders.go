package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// defaultHTTPClient is a reusable HTTP client used when no custom client is provided.
var defaultHTTPClient = NewHTTPClient(10 * time.Second)

// httpClient implements HTTPClient.
type httpClient struct {
	client *http.Client
}

// NewHTTPClient creates an HTTPClient with the given timeout.
func NewHTTPClient(timeout time.Duration) HTTPClient {
	return &httpClient{
		client: &http.Client{Timeout: timeout},
	}
}

// PostJSON sends a JSON POST request and returns the response body.
func (c *httpClient) PostJSON(ctx context.Context, url string, body interface{}) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("alert: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("alert: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alert: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("alert: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("alert: webhook returned %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// signV1 generates a HMAC-SHA256 signature for DingTalk webhook auth.
// Algorithm: timestamp + "\n" + secret → HMAC-SHA256 → Base64.
func signV1(secret string, timestamp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// FeishuSender sends alerts to Feishu (飞书) custom robot webhooks.
type FeishuSender struct {
	config ChannelConfig
	client HTTPClient
}

// NewFeishuSender creates a new FeishuSender.
func NewFeishuSender(cfg ChannelConfig, client HTTPClient) *FeishuSender {
	return &FeishuSender{config: cfg, client: client}
}

// Channel returns the channel type.
func (s *FeishuSender) Channel() Channel { return ChannelFeishu }

// feishuCard is the Feishu interactive card payload.
type feishuCard struct {
	MsgType string             `json:"msg_type"`
	Card    *feishuCardContent `json:"card,omitempty"`
}

type feishuCardContent struct {
	Header     feishuHeader    `json:"header"`
	ELElements []feishuElement `json:"elements"`
}

type feishuHeader struct {
	Title feishuText `json:"title"`
}

type feishuText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type feishuElement struct {
	Tag     string        `json:"tag"`
	Content string        `json:"content,omitempty"`
	Fields  []feishuField `json:"fields,omitempty"`
}

type feishuField struct {
	IsShort bool       `json:"is_short"`
	Text    feishuText `json:"text"`
}

// Send delivers the alert to Feishu via interactive card.
func (s *FeishuSender) Send(ctx context.Context, msg AlertMessage) error {
	title := msg.Title
	if title == "" {
		title = fmt.Sprintf("[%s] %s", msg.Severity, msg.ServiceName)
	}

	body := msg.Body
	if body == "" {
		body = buildBody(msg)
	}

	card := &feishuCard{
		MsgType: "interactive",
		Card: &feishuCardContent{
			Header: feishuHeader{
				Title: feishuText{Tag: "plain_text", Content: title},
			},
			ELElements: []feishuElement{
				{Tag: "div", Content: body},
				{Tag: "hr"},
				{
					Tag: "div",
					Fields: []feishuField{
						{IsShort: true, Text: feishuText{Tag: "lark_md", Content: fmt.Sprintf("**服务:** %s", msg.ServiceName)}},
						{IsShort: true, Text: feishuText{Tag: "lark_md", Content: fmt.Sprintf("**级别:** %s", msg.Severity)}},
						{IsShort: true, Text: feishuText{Tag: "lark_md", Content: fmt.Sprintf("**时间:** %s", msg.Timestamp.Format("01-02 15:04:05"))}},
						{IsShort: true, Text: feishuText{Tag: "lark_md", Content: fmt.Sprintf("**错误率:** %.1f%%", msg.ErrorRate*100)}},
					},
				},
			},
		},
	}

	_, err := s.client.PostJSON(ctx, s.config.WebhookURL, card)
	return err
}

// DingTalkSender sends alerts to DingTalk (钉钉) custom robot webhooks.
type DingTalkSender struct {
	config ChannelConfig
	client HTTPClient
}

// NewDingTalkSender creates a new DingTalkSender.
func NewDingTalkSender(cfg ChannelConfig, client HTTPClient) *DingTalkSender {
	return &DingTalkSender{config: cfg, client: client}
}

// Channel returns the channel type.
func (s *DingTalkSender) Channel() Channel { return ChannelDingTalk }

// dingtalkPayload is the DingTalk webhook message payload.
type dingtalkPayload struct {
	MsgType    string              `json:"msgtype"`
	Markdown   *dingtalkMarkdown   `json:"markdown,omitempty"`
	ActionCard *dingtalkActionCard `json:"actionCard,omitempty"`
}

type dingtalkMarkdown struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type dingtalkActionCard struct {
	Title          string `json:"title"`
	Text           string `json:"text"`
	BtnOrientation string `json:"btnOrientation"`
	SingleTitle    string `json:"singleTitle"`
	SingleURL      string `json:"singleURL"`
}

// Send delivers the alert to DingTalk via markdown message.
func (s *DingTalkSender) Send(ctx context.Context, msg AlertMessage) error {
	title := msg.Title
	if title == "" {
		title = fmt.Sprintf("[%s] %s", msg.Severity, msg.ServiceName)
	}

	body := msg.Body
	if body == "" {
		body = buildBody(msg)
	}

	md := fmt.Sprintf("## %s\n\n%s\n\n---\n**服务:** %s | **级别:** %s\n**错误率:** %.1f%% | **时间:** %s",
		title, body, msg.ServiceName, msg.Severity, msg.ErrorRate*100, msg.Timestamp.Format("01-02 15:04:05"))

	if len(msg.AffectedProviders) > 0 {
		md += fmt.Sprintf("\n**影响 Provider:** %v", msg.AffectedProviders)
	}
	if msg.Latency > 0 {
		md += fmt.Sprintf("\n**耗时:** %s", msg.Latency)
	}

	payload := &dingtalkPayload{
		MsgType: "actionCard",
		ActionCard: &dingtalkActionCard{
			Title:          title,
			Text:           md,
			BtnOrientation: "0",
			SingleTitle:    "查看详情",
			SingleURL:      s.config.WebhookURL,
		},
	}

	url := s.config.WebhookURL
	if s.config.SignSecret != "" {
		timestamp := time.Now().UnixMilli()
		sign := signV1(s.config.SignSecret, timestamp)
		url = fmt.Sprintf("%s&timestamp=%d&sign=%s", s.config.WebhookURL, timestamp, sign)
	}

	_, err := s.client.PostJSON(ctx, url, payload)
	return err
}

// WeComSender sends alerts to WeCom (企业微信) custom robot webhooks.
type WeComSender struct {
	config ChannelConfig
	client HTTPClient
}

// NewWeComSender creates a new WeComSender.
func NewWeComSender(cfg ChannelConfig, client HTTPClient) *WeComSender {
	return &WeComSender{config: cfg, client: client}
}

// Channel returns the channel type.
func (s *WeComSender) Channel() Channel { return ChannelWeCom }

// wecomPayload is the WeCom webhook message payload.
type wecomPayload struct {
	MsgType  string         `json:"msgtype"`
	Markdown *wecomMarkdown `json:"markdown,omitempty"`
	Text     *wecomText     `json:"text,omitempty"`
}

type wecomMarkdown struct {
	Content string `json:"content"`
}

type wecomText struct {
	Content string `json:"content"`
}

// Send delivers the alert to WeCom via markdown message.
func (s *WeComSender) Send(ctx context.Context, msg AlertMessage) error {
	title := msg.Title
	if title == "" {
		title = fmt.Sprintf("[%s] %s", msg.Severity, msg.ServiceName)
	}

	body := msg.Body
	if body == "" {
		body = buildBody(msg)
	}

	md := fmt.Sprintf("## %s\n%s\n\n> **服务:** %s\n> **级别:** %s\n> **错误率:** %.1f%%\n> **时间:** %s",
		title, body, msg.ServiceName, msg.Severity, msg.ErrorRate*100, msg.Timestamp.Format("01-02 15:04:05"))

	if len(msg.AffectedProviders) > 0 {
		md += fmt.Sprintf("\n> **影响 Provider:** %v", msg.AffectedProviders)
	}
	if msg.Latency > 0 {
		md += fmt.Sprintf("\n> **耗时:** %s", msg.Latency)
	}

	payload := &wecomPayload{
		MsgType:  "markdown",
		Markdown: &wecomMarkdown{Content: md},
	}

	_, err := s.client.PostJSON(ctx, s.config.WebhookURL, payload)
	return err
}

// buildBody renders a standard alert body from the AlertMessage fields.
func buildBody(msg AlertMessage) string {
	body := fmt.Sprintf("服务异常告警\n\n**服务**: %s\n**级别**: %s\n**错误率**: %.1f%%",
		msg.ServiceName, msg.Severity, msg.ErrorRate*100)

	if msg.Latency > 0 {
		body += fmt.Sprintf("\n**耗时**: %s", msg.Latency)
	}
	if len(msg.AffectedProviders) > 0 {
		body += fmt.Sprintf("\n**影响 Provider**: %v", msg.AffectedProviders)
	}
	body += fmt.Sprintf("\n**时间**: %s", msg.Timestamp.Format("2006-01-02 15:04:05 MST"))
	return body
}

// Ensure compile-time interface satisfaction.
var _ Sender = (*FeishuSender)(nil)
var _ Sender = (*DingTalkSender)(nil)
var _ Sender = (*WeComSender)(nil)
