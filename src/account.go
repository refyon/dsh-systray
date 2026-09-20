// account.go：dsh-connect 账号 API 客户端（登录、账号信息、操作记录同步）。
//
// 契约见 apps/dsh-connect/docs/API.md（服务端已上线 https://api.instantserv.ccwu.cc）。
// 只依赖标准库；错误按服务端 `error.code` 分支，文案由 i18n 层本地化。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultAccountAPIBase 正式服务地址（可被 config.json 的 accountApiBase 覆盖）。
const defaultAccountAPIBase = "https://api.instantserv.ccwu.cc"

const (
	accountRequestTimeout = 10 * time.Second
	// accountRetryAttempts 网络错误/5xx 的重试次数（4xx 立即返回，不重试）。
	accountRetryAttempts = 3
	// accountOpsPageSize 单次增量拉取的条数（服务端上限 500）。
	accountOpsPageSize = 200
	// accountOpsMaxBatch 单次上报的条数（服务端上限 100）。
	accountOpsMaxBatch = 100
)

// 服务端错误码（与 dsh-connect src/types.ts 一致；客户端只依赖这些）。
const (
	accErrInvalidEmail = "invalid_email"
	accErrRateLimited  = "rate_limited"
	accErrOTPInvalid   = "otp_invalid"
	accErrOTPExpired   = "otp_expired"
	accErrOTPTooMany   = "otp_too_many_attempts"
	accErrUnauthorized = "unauthorized"
	accErrMailFailed   = "mail_send_failed"
	// accErrNetwork 客户端侧错误码：连接失败/超时（服务端不会返回）。
	accErrNetwork = "network"
)

// accountError 服务端错误信封或网络错误。
type accountError struct {
	Code    string
	Message string
	Status  int
}

func (e *accountError) Error() string {
	return fmt.Sprintf("account api: %s (%s, http %d)", e.Code, e.Message, e.Status)
}

// accountErrorCode 从任意 error 提取稳定错误码（便于 UI 分支与测试断言）。
func accountErrorCode(err error) string {
	var ae *accountError
	if errors.As(err, &ae) {
		return ae.Code
	}
	if err != nil {
		return accErrNetwork
	}
	return ""
}

// accountAPIBase 返回生效的服务地址：config.json 的 accountApiBase → 环境变量 → 默认正式域名。
func accountAPIBase() string {
	if v := strings.TrimSpace(accountAPIBaseOverride); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultAccountAPIBase
}

// ---------- 报文类型（字段名与服务端契约逐字一致） ----------

type accountCodeResult struct {
	ExpiresInSec   int `json:"expiresInSec"`
	ResendAfterSec int `json:"resendAfterSec"`
}

type accountUser struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	CreatedAt int64  `json:"createdAt"`
}

type accountDeviceInfo struct {
	Name       string `json:"name,omitempty"`
	Platform   string `json:"platform,omitempty"`
	AppVersion string `json:"appVersion,omitempty"`
}

type accountSession struct {
	Token          string      `json:"token"`
	TokenExpiresAt int64       `json:"tokenExpiresAt"`
	DeviceID       string      `json:"deviceId"`
	User           accountUser `json:"user"`
}

type accountDevice struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	AppVersion string `json:"appVersion"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt"`
	Current    bool   `json:"current"`
}

type accountMe struct {
	User    accountUser     `json:"user"`
	Devices []accountDevice `json:"devices"`
}

// accountOp 一条待上报的操作记录（Value 为任意 JSON 值）。
type accountOp struct {
	OpID  string          `json:"opId"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// accountOpRecord 服务端返回的一条操作记录。
type accountOpRecord struct {
	Seq       int64           `json:"seq"`
	OpID      string          `json:"opId"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	DeviceID  string          `json:"deviceId"`
	UpdatedAt int64           `json:"updatedAt"`
}

type accountOpsPage struct {
	Ops     []accountOpRecord `json:"ops"`
	Cursor  int64             `json:"cursor"`
	HasMore bool              `json:"hasMore"`
}

type accountOpsReport struct {
	Accepted   int   `json:"accepted"`
	Duplicates int   `json:"duplicates"`
	Cursor     int64 `json:"cursor"`
}

// ---------- 客户端 ----------

type accountClient struct {
	base string
	http *http.Client
	// backoff 退避时长（可注入；测试置 0，生产为 300ms × 2^n）。
	backoff func(attempt int) time.Duration
}

func newAccountClient(base string) *accountClient {
	if strings.TrimSpace(base) == "" {
		base = accountAPIBase()
	}
	return &accountClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: accountRequestTimeout},
		backoff: func(attempt int) time.Duration {
			return 300 * time.Millisecond * time.Duration(1<<uint(attempt))
		},
	}
}

// do 发起 JSON 请求：网络错误与 5xx 指数退避重试（≤3 次），4xx 直接把错误信封转成 *accountError。
func (c *accountClient) do(ctx context.Context, method, path, token string, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("account: 序列化请求失败: %w", err)
		}
		payload = b
	}

	var lastErr error
	for attempt := 0; attempt < accountRetryAttempts; attempt++ {
		if attempt > 0 && c.backoff != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.backoff(attempt - 1)):
			}
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
		if err != nil {
			return fmt.Errorf("account: 构造请求失败: %w", err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		res, err := c.http.Do(req)
		if err != nil {
			lastErr = &accountError{Code: accErrNetwork, Message: err.Error()}
			continue // 网络错误：重试
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if readErr != nil {
			lastErr = &accountError{Code: accErrNetwork, Message: readErr.Error(), Status: res.StatusCode}
			continue
		}

		if res.StatusCode >= 500 {
			lastErr = &accountError{Code: "server_error", Message: strings.TrimSpace(string(data)), Status: res.StatusCode}
			continue // 5xx：重试
		}
		if res.StatusCode >= 400 {
			// 4xx：服务端已给出稳定错误码，直接返回（含 429 限流，交由 UI 提示等待）
			return decodeAccountError(res.StatusCode, data)
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("account: 解析响应失败: %w", err)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = &accountError{Code: accErrNetwork, Message: "请求失败"}
	}
	return lastErr
}

// decodeAccountError 解析错误信封 `{"error":{"code","message"}}`；兜底用 HTTP 状态码。
func decodeAccountError(status int, data []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && env.Error.Code != "" {
		return &accountError{Code: env.Error.Code, Message: env.Error.Message, Status: status}
	}
	return &accountError{Code: "http_error", Message: strings.TrimSpace(string(data)), Status: status}
}

// RequestCode 请求邮箱验证码（服务端对未注册邮箱同样返回 202）。
func (c *accountClient) RequestCode(ctx context.Context, email, locale string) (accountCodeResult, error) {
	var out accountCodeResult
	body := map[string]string{"email": strings.TrimSpace(email)}
	if locale != "" {
		body["locale"] = locale
	}
	err := c.do(ctx, http.MethodPost, "/v1/auth/otp/request", "", body, &out)
	return out, err
}

// Verify 校验验证码并取得会话（首次成功即注册账号）。
func (c *accountClient) Verify(ctx context.Context, email, code string, device accountDeviceInfo) (accountSession, error) {
	var out accountSession
	body := map[string]any{
		"email":  strings.TrimSpace(email),
		"code":   strings.TrimSpace(code),
		"device": device,
	}
	err := c.do(ctx, http.MethodPost, "/v1/auth/otp/verify", "", body, &out)
	return out, err
}

// Me 读取当前账号与设备列表（同时用于校验令牌是否仍然有效）。
func (c *accountClient) Me(ctx context.Context, token string) (accountMe, error) {
	var out accountMe
	err := c.do(ctx, http.MethodGet, "/v1/me", token, nil, &out)
	return out, err
}

// Logout 撤销当前令牌。
func (c *accountClient) Logout(ctx context.Context, token string) error {
	return c.do(ctx, http.MethodPost, "/v1/auth/logout", token, nil, nil)
}

// ReportOps 幂等上报操作记录（服务端按 opId 去重）。
func (c *accountClient) ReportOps(ctx context.Context, token string, ops []accountOp) (accountOpsReport, error) {
	var out accountOpsReport
	if len(ops) == 0 {
		return out, nil
	}
	if len(ops) > accountOpsMaxBatch {
		return out, fmt.Errorf("account: 单次上报超限（%d > %d）", len(ops), accountOpsMaxBatch)
	}
	err := c.do(ctx, http.MethodPost, "/v1/ops/report", token, map[string]any{"ops": ops}, &out)
	return out, err
}

// OpsSince 按游标增量拉取操作记录。
func (c *accountClient) OpsSince(ctx context.Context, token string, cursor int64, limit int) (accountOpsPage, error) {
	var out accountOpsPage
	if limit <= 0 || limit > 500 {
		limit = accountOpsPageSize
	}
	path := fmt.Sprintf("/v1/ops/since?cursor=%d&limit=%d", cursor, limit)
	err := c.do(ctx, http.MethodGet, path, token, nil, &out)
	return out, err
}
