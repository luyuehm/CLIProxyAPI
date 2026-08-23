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
	"net/url"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
)

// WebhookSender 将告警消息发送到飞书/钉钉/企微 webhook。
type WebhookSender struct {
	client  *http.Client
	timeout time.Duration
}

func NewWebhookSender(timeout time.Duration) *WebhookSender {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &WebhookSender{
		client:  &http.Client{Timeout: timeout},
		timeout: timeout,
	}
}

// Send 根据通道平台构造 payload 并发送，返回消息发送摘要（用于事件记录）。
func (s *WebhookSender) Send(ctx context.Context, channel AlertChannel, message string) (string, error) {
	payload, err := buildPayload(channel, message)
	if err != nil {
		return "", err
	}

	finalURL := channel.WebhookURL
	if channel.Platform == string(PlatformDingTalk) && channel.Secret != "" {
		finalURL = dingtalkSignedURL(channel.WebhookURL, channel.Secret)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, finalURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// 钉钉/飞书成功都返回 200；企微成功返回 200。非 2xx 需要显式处理。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, string(responseBody))
	}

	// 平台级成功判断：飞书/钉钉可能返回 200 且 code!=0。
	platformErr := checkPlatformResponse(channel.Platform, responseBody)
	if platformErr != nil {
		return "", platformErr
	}

	logrus.WithField("channel_id", channel.ID).WithField("platform", channel.Platform).Info("alert webhook sent")
	return summaryOf(channel, payload), nil
}

// buildPayload 构造各平台兼容的 webhook payload。
func buildPayload(channel AlertChannel, message string) (map[string]interface{}, error) {
	switch channel.Platform {
	case string(PlatformFeishu):
		return map[string]interface{}{
			"msg_type": "text",
			"content":  map[string]interface{}{"text": message},
		}, nil
	case string(PlatformDingTalk):
		return map[string]interface{}{
			"msgtype": "text",
			"text":    map[string]interface{}{"content": message},
		}, nil
	case string(PlatformWeCom):
		return map[string]interface{}{
			"msgtype": "text",
			"text":    map[string]interface{}{"content": message},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported platform %q", channel.Platform)
	}
}

// dingtalkSignedURL 附加钉钉机器人所需的 timestamp/sign 安全参数。
func dingtalkSignedURL(webhookURL, secret string) string {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	stringToSign := timestamp + "\n" + secret
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write([]byte(stringToSign))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(digest.Sum(nil)))
	return fmt.Sprintf("%s%stimestamp=%s&sign=%s",
		webhookURL, webhookSeparator(webhookURL), timestamp, sign)
}

func webhookSeparator(raw string) string {
	if _, err := url.Parse(raw); err == nil && containsQuery(raw) {
		return "&"
	}
	return "?"
}

func containsQuery(raw string) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '?' {
			return true
		}
	}
	return false
}

// checkPlatformResponse 校验飞书/钉钉等返回体内的 code。
func checkPlatformResponse(platform string, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	var parsed struct {
		Code    int    `json:"code"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	switch platform {
	case string(PlatformFeishu):
		if parsed.Code != 0 {
			return fmt.Errorf("feishu webhook error code=%d msg=%s", parsed.Code, parsed.Msg)
		}
	case string(PlatformDingTalk):
		if parsed.ErrCode != 0 {
			return fmt.Errorf("dingtalk webhook error errcode=%d errmsg=%s", parsed.ErrCode, parsed.ErrMsg)
		}
	case string(PlatformWeCom):
		if parsed.ErrCode != 0 {
			return fmt.Errorf("wecom webhook error errcode=%d errmsg=%s", parsed.ErrCode, parsed.ErrMsg)
		}
	}
	return nil
}

func summaryOf(channel AlertChannel, payload map[string]interface{}) string {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(bytes)
}