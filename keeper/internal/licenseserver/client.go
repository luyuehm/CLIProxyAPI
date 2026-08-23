package licenseserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是 keeper 侧连接 license server 的远程验证客户端。
// 远程不可达时由调用方按离线宽限期降级（见 OfflineGrace 模式判断）。
type Client struct {
	baseURL    string
	machineID  string
	httpClient *http.Client
}

func NewClient(baseURL, machineID string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("license server base URL is required")
	}
	if strings.TrimSpace(machineID) == "" {
		return nil, fmt.Errorf("machine id is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		machineID: strings.TrimSpace(machineID),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
	return c, nil
}

type RemoteResult struct {
	Valid          bool
	LeaseExpiresAt string
	Message        string
}

// Activate 尝试远程激活。返回 (result, remoteReachable, err)。
// remoteReachable=false 表示网络不可达，调用方应启用离线宽限期。
func (c *Client) Activate(ctx context.Context, licenseKey string) (RemoteResult, bool, error) {
	payload := ActivateRequest{LicenseKey: licenseKey, MachineID: c.machineID}
	var response ActivateResponse
	reachable, err := c.postJSON(ctx, "/activate", payload, &response)
	if err != nil {
		return RemoteResult{}, reachable, err
	}
	return RemoteResult{
		Valid:          response.Valid,
		LeaseExpiresAt: response.LeaseExpiresAt,
		Message:        response.Message,
	}, true, nil
}

// Heartbeat 尝试远程心跳续期。
func (c *Client) Heartbeat(ctx context.Context, licenseKey string) (RemoteResult, bool, error) {
	payload := HeartbeatRequest{LicenseKey: licenseKey, MachineID: c.machineID}
	var response HeartbeatResponse
	reachable, err := c.postJSON(ctx, "/heartbeat", payload, &response)
	if err != nil {
		return RemoteResult{}, reachable, err
	}
	return RemoteResult{
		Valid:          response.Valid,
		LeaseExpiresAt: response.LeaseExpiresAt,
		Message:        response.Message,
	}, true, nil
}

// Check 查询远程许可证状态（只读）。
func (c *Client) Check(ctx context.Context, licenseKey string) (RemoteResult, bool, error) {
	url := fmt.Sprintf("%s/status?license_key=%s", c.baseURL, licenseKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RemoteResult{}, false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RemoteResult{}, false, nil // 网络错误按不可达处理
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return RemoteResult{}, true, fmt.Errorf("license server returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var response StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return RemoteResult{}, true, err
	}
	return RemoteResult{
		Valid:          response.Valid,
		LeaseExpiresAt: response.LeaseExpiresAt,
		Message:        response.Message,
	}, true, nil
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, target any) (bool, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return true, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return true, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, nil // 网络不可达
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return true, fmt.Errorf("license server returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if target != nil {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			return true, err
		}
	}
	return true, nil
}

// OfflineGraceStatus 描述远程不可达时的离线宽限期判定结果。
type OfflineGraceStatus struct {
	// Mode 为 offline / grace / expired。
	Mode string
	// Message 是给日志/用户的可读说明。
	Message string
	// GraceLeft 是剩余宽限期；Mode=grace 时有效。
	GraceLeft time.Duration
}

// EvaluateOfflineGrace 在远程不可达时按最近一次成功验证时间决定是否放行。
// lastSuccess 为最近一次远程验证成功的时间点；grace 为允许的离线宽限期。
// 返回 expired=true 时调用方必须拒绝启动。
func EvaluateOfflineGrace(lastSuccess time.Time, grace time.Duration) OfflineGraceStatus {
	if grace <= 0 {
		return OfflineGraceStatus{
			Mode:    "expired",
			Message: "offline grace disabled; refusing to start",
		}
	}
	if lastSuccess.IsZero() {
		return OfflineGraceStatus{
			Mode:    "expired",
			Message: "never validated remotely; network unreachable and no prior success",
		}
	}
	elapsed := time.Since(lastSuccess)
	if elapsed > grace {
		return OfflineGraceStatus{
			Mode:    "expired",
			Message: fmt.Sprintf("offline grace period (%v) exhausted", grace),
		}
	}
	return OfflineGraceStatus{
		Mode:      "grace",
		Message:   fmt.Sprintf("remote license server unreachable; %v of offline grace remaining", grace-elapsed),
		GraceLeft: grace - elapsed,
	}
}