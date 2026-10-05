// account_files_client.go：文件同步端点客户端（docs/API.md 端点 13-20）。
//
// 与 account.go 的分工：普通 API 走 c.do（10s 超时、JSON 信封）；文件内容走 c.doRaw
// （二进制体、2 分钟超时、402/409 等业务码仍按 error.code 分支）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// fileSyncTransferTimeout 单文件上传/下载超时（10 MiB 上限，慢网也够）。
const fileSyncTransferTimeout = 2 * time.Minute

// fileSyncObjectQuery 对象定位参数（与服务端 query 名逐字一致）。
func fileSyncObjectQuery(entryID, relPath string) string {
	v := url.Values{}
	v.Set("entryId", entryID)
	v.Set("relPath", relPath)
	return v.Encode()
}

// doRaw 发起原始请求（二进制体/响应）：网络错误与 5xx 退避重试，4xx 转成 *accountError。
// PUT 上传是幂等的（同内容重传覆盖同一条目路径），重试安全。
func (c *accountClient) doRaw(ctx context.Context, method, path, token string, body []byte, headers map[string]string) (*http.Response, error) {
	client := c.fileHTTP
	if client == nil {
		client = newHTTPClient(fileSyncTransferTimeout)
	}
	var lastErr error
	for attempt := 0; attempt < accountRetryAttempts; attempt++ {
		if attempt > 0 && c.backoff != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.backoff(attempt - 1)):
			}
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
		if err != nil {
			return nil, fmt.Errorf("account: 构造请求失败: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			lastErr = &accountError{Code: accErrNetwork, Message: err.Error()}
			continue
		}
		if res.StatusCode >= 500 {
			data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			res.Body.Close()
			lastErr = &accountError{Code: "server_error", Message: strings.TrimSpace(string(data)), Status: res.StatusCode}
			continue
		}
		if res.StatusCode >= 400 {
			data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			return nil, decodeAccountError(res.StatusCode, data)
		}
		return res, nil
	}
	if lastErr == nil {
		lastErr = &accountError{Code: accErrNetwork, Message: "请求失败"}
	}
	return nil, lastErr
}

// FilesQuota 读取容量快照。
func (c *accountClient) FilesQuota(ctx context.Context, token string) (fileQuota, error) {
	var out fileQuota
	err := c.do(ctx, http.MethodGet, "/v1/files/quota", token, nil, &out)
	return out, err
}

// FilesCreateEntry 新建同步条目（id 由客户端生成；显示名账号内唯一）。
func (c *accountClient) FilesCreateEntry(ctx context.Context, token string, e fileSyncEntryBody) (fileSyncRemoteEntry, error) {
	var out fileSyncRemoteEntry
	err := c.do(ctx, http.MethodPost, "/v1/files/entries", token, e, &out)
	return out, err
}

// FilesDeleteEntry 删除同步条目（其它设备按「服务端没有该条目」收敛）。
func (c *accountClient) FilesDeleteEntry(ctx context.Context, token, entryID string) error {
	path := "/v1/files/entries/" + url.PathEscape(entryID)
	return c.do(ctx, http.MethodDelete, path, token, nil, nil)
}

// FilesSync 对账（服务端只读计算，返回本机应执行的动作）。
func (c *accountClient) FilesSync(ctx context.Context, token string, req fileSyncRequest) (fileSyncResponse, error) {
	var out fileSyncResponse
	err := c.do(ctx, http.MethodPost, "/v1/files/sync", token, req, &out)
	return out, err
}

// FilesUpload 上传/替换文件内容（体积与摘要走请求头）。
func (c *accountClient) FilesUpload(
	ctx context.Context,
	token, entryID, relPath string,
	data []byte,
	sha string,
	mtime int64,
) (fileUploadResponse, error) {
	var out fileUploadResponse
	res, err := c.doRaw(ctx, http.MethodPut, "/v1/files/objects?"+fileSyncObjectQuery(entryID, relPath), token, data, map[string]string{
		"Content-Type":  "application/octet-stream",
		"x-file-size":   strconv.FormatInt(int64(len(data)), 10),
		"x-file-sha256": sha,
		"x-file-mtime":  strconv.FormatInt(mtime, 10),
	})
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return out, &accountError{Code: accErrNetwork, Message: err.Error()}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("account: 解析上传响应失败: %w", err)
	}
	return out, nil
}

// FilesDownload 下载文件内容（服务端单文件 ≤10 MiB，多读 1 字节用于越界判断）。
func (c *accountClient) FilesDownload(ctx context.Context, token, entryID, relPath string) ([]byte, error) {
	res, err := c.doRaw(ctx, http.MethodGet, "/v1/files/objects?"+fileSyncObjectQuery(entryID, relPath), token, nil, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxFileSyncBytes+1))
	if err != nil {
		return nil, &accountError{Code: accErrNetwork, Message: err.Error()}
	}
	if int64(len(data)) > maxFileSyncBytes {
		return nil, fmt.Errorf("account: 下载内容超出单文件上限（%d 字节）", maxFileSyncBytes)
	}
	return data, nil
}

// FilesDeleteObject 删除文件（服务端打墓碑并下发给其它设备），返回最新容量。
func (c *accountClient) FilesDeleteObject(ctx context.Context, token, entryID, relPath string) (fileQuota, error) {
	var out fileQuota
	err := c.do(ctx, http.MethodDelete, "/v1/files/objects?"+fileSyncObjectQuery(entryID, relPath), token, nil, &out)
	return out, err
}
