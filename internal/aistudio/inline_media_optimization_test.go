package aistudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestExtractInlineMediaKeysAndStickyRouting 测试内联附件提取与带内联缓存命中加权的轮询粘性调度
func TestExtractInlineMediaKeysAndStickyRouting(t *testing.T) {
	img1Data := []byte("image-data-1")
	img2Data := []byte("image-data-2")
	h1 := sha256.Sum256(img1Data)
	h2 := sha256.Sum256(img2Data)

	contents := []Content{
		{
			Parts: []Part{
				{Text: "hello"},
				{InlineData: &Blob{MIME: "image/png", Data: img1Data}, DisplayName: "img1.png"},
				{File: &FileRef{ID: "file-1"}},
			},
		},
		{
			Parts: []Part{
				// 重复项应被去重
				{InlineData: &Blob{MIME: "image/png", Data: img1Data}, DisplayName: "img1.png"},
				{InlineData: &Blob{MIME: "image/jpeg", Data: img2Data}, DisplayName: "img2.jpg"},
				{InlineData: nil},
				{InlineData: &Blob{MIME: "", Data: []byte("empty")}},
			},
		},
	}

	keys := ExtractInlineMediaKeys(contents)
	if len(keys) != 2 {
		t.Fatalf("expected 2 unique keys, got %d", len(keys))
	}
	if keys[0].Name != "img1.png" || keys[0].MIME != "image/png" {
		t.Errorf("unexpected key[0]: %+v", keys[0])
	}
	if keys[1].Name != "img2.jpg" || keys[1].MIME != "image/jpeg" {
		t.Errorf("unexpected key[1]: %+v", keys[1])
	}

	// 测试调度池粘性排序
	now := time.Now().UTC()
	model := Model{ID: "gemini-2.5-flash", Methods: []string{"generateContent"}}
	acc1 := &Account{ID: "acc-1", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, runtime: accountRuntimeState{Resources: make(map[string]ResourceBinding)}}
	acc2 := &Account{ID: "acc-2", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, runtime: accountRuntimeState{Resources: make(map[string]ResourceBinding)}}
	acc3 := &Account{ID: "acc-3", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, runtime: accountRuntimeState{Resources: make(map[string]ResourceBinding)}}

	// acc2 缓存了 img1
	acc2.runtime.Resources["drive-img-1"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now,
	}
	// acc3 缓存了 img1 和 img2
	acc3.runtime.Resources["drive-img-1-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now,
	}
	acc3.runtime.Resources["drive-img-2-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[1].MIME, SHA256: keys[1].SHA256, DisplayName: keys[1].Name, UsedAt: now,
	}

	pool := NewAccountPool([]*Account{acc1, acc2, acc3}, 1)
	pool.SetRoutingStrategy("round-robin")

	indices, err := pool.selectionIndicesLocked(AccountSelection{
		ModelID:         "gemini-2.5-flash",
		InlineMediaKeys: keys,
	})
	if err != nil {
		t.Fatalf("selectionIndicesLocked failed: %v", err)
	}

	// acc3 (2 命中) 应排在最前，acc2 (1 命中) 其次，acc1 (0 命中) 最后
	if len(indices) != 3 {
		t.Fatalf("expected 3 indices, got %d", len(indices))
	}
	if pool.accounts[indices[0]].ID != "acc-3" {
		t.Errorf("expected acc-3 first, got %s", pool.accounts[indices[0]].ID)
	}
	if pool.accounts[indices[1]].ID != "acc-2" {
		t.Errorf("expected acc-2 second, got %s", pool.accounts[indices[1]].ID)
	}
	if pool.accounts[indices[2]].ID != "acc-1" {
		t.Errorf("expected acc-1 third, got %s", pool.accounts[indices[2]].ID)
	}

	// 测试过期时间: 超过 inlineMediaIdle 的缓存不应算作命中
	acc3.runtime.Resources["drive-img-1-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now.Add(-2 * time.Hour),
	}
	acc3.runtime.Resources["drive-img-2-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[1].MIME, SHA256: keys[1].SHA256, DisplayName: keys[1].Name, UsedAt: now.Add(-2 * time.Hour),
	}
	hitsAcc3 := pool.inlineMediaHitsLocked(acc3, keys, now)
	if hitsAcc3 != 0 {
		t.Errorf("expected 0 hits for expired items, got %d", hitsAcc3)
	}
	_ = h1
	_ = h2
}

// TestBindingPromptHashEquivalence 验证流式 SHA-256 与原始 bindingPrompt 字符串哈希的严格等价性
func TestBindingPromptHashEquivalence(t *testing.T) {
	testCases := []struct {
		name    string
		request GenerateRequest
	}{
		{
			name: "pure text",
			request: GenerateRequest{
				Contents: []Content{
					{Parts: []Part{{Text: "Hello world"}}},
					{Parts: []Part{{Text: "Second turn"}, {Text: "with multiple parts"}}},
				},
			},
		},
		{
			name: "text and inline images",
			request: GenerateRequest{
				Contents: []Content{
					{
						Parts: []Part{
							{Text: "Describe this image:"},
							{InlineData: &Blob{MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRtestdata123456789")}, DisplayName: "test.png"},
							{Text: "And this file:"},
							{File: &FileRef{ID: "drive-file-abc-123"}},
						},
					},
				},
			},
		},
		{
			name: "multiple large inline blobs",
			request: GenerateRequest{
				Contents: []Content{
					{
						Parts: []Part{
							{InlineData: &Blob{MIME: "image/jpeg", Data: bytes.Repeat([]byte{0x42, 0x43, 0x44, 0x45}, 10000)}},
							{InlineData: &Blob{MIME: "image/webp", Data: bytes.Repeat([]byte{0x10, 0x20, 0x30, 0x40}, 5000)}},
							{Text: "Some suffix text"},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			strPrompt, err := bindingPrompt(tc.request)
			if err != nil {
				t.Fatalf("bindingPrompt failed: %v", err)
			}
			expectedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(strPrompt)))

			gotHash, err := bindingPromptHash(tc.request)
			if err != nil {
				t.Fatalf("bindingPromptHash failed: %v", err)
			}

			if gotHash != expectedHash {
				t.Errorf("hash mismatch:\nexpected: %s\ngot:      %s", expectedHash, gotHash)
			}
		})
	}
}

type testRPCTransport struct {
	mu         sync.Mutex
	callCounts map[string]int
	handler    func(RPCRequest) (*RPCResponse, error)
}

func (m *testRPCTransport) Do(ctx context.Context, req RPCRequest) (*RPCResponse, error) {
	m.mu.Lock()
	if m.callCounts == nil {
		m.callCounts = make(map[string]int)
	}
	m.callCounts[req.Method]++
	m.mu.Unlock()
	if m.handler != nil {
		return m.handler(req)
	}
	return &RPCResponse{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`["mock"]`))}, nil
}

// TestClientDriveCachingAndNoAuthMemoization 测试 AccessToken、appFolder 缓存及 unauthorized_client 快速失败记忆
func TestClientDriveCachingAndNoAuthMemoization(t *testing.T) {
	ctx := context.Background()

	transport := &testRPCTransport{
		handler: func(req RPCRequest) (*RPCResponse, error) {
			switch req.Method {
			case "GenerateAccessToken":
				if req.AccountID == "no-auth-user" {
					return nil, &RPCError{
						Method:     "GenerateAccessToken",
						StatusCode: http.StatusUnauthorized,
						Code:       16,
						Message:    "OAuth error: unauthorized_client.",
					}
				}
				return &RPCResponse{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
					Body:       io.NopCloser(strings.NewReader(`["mock-access-token-xyz"]`)),
				}, nil
			case "GetAppFolder":
				return &RPCResponse{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
					Body:       io.NopCloser(strings.NewReader(`["app-folder-id-123"]`)),
				}, nil
			}
			return &RPCResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
				Body:       io.NopCloser(strings.NewReader(`["ok"]`)),
			}, nil
		},
	}

	client := NewClient(ClientOptions{Transport: transport})

	// 1. 测试正常用户的 AccessToken 缓存
	token1, err := client.GenerateAccessToken(ctx, "valid-user")
	if err != nil || token1 != "mock-access-token-xyz" {
		t.Fatalf("first GenerateAccessToken failed: token=%s err=%v", token1, err)
	}
	token2, err := client.GenerateAccessToken(ctx, "valid-user")
	if err != nil || token2 != "mock-access-token-xyz" {
		t.Fatalf("second GenerateAccessToken failed: token=%s err=%v", token2, err)
	}

	transport.mu.Lock()
	generateCalls := transport.callCounts["GenerateAccessToken"]
	transport.mu.Unlock()
	if generateCalls != 1 {
		t.Errorf("expected exactly 1 GenerateAccessToken call, got %d", generateCalls)
	}

	// 2. 测试 appFolder 缓存
	folder1, err := client.appFolder(ctx, "valid-user")
	if err != nil || folder1 != "app-folder-id-123" {
		t.Fatalf("first appFolder failed: folder=%s err=%v", folder1, err)
	}
	folder2, err := client.appFolder(ctx, "valid-user")
	if err != nil || folder2 != "app-folder-id-123" {
		t.Fatalf("second appFolder failed: folder=%s err=%v", folder2, err)
	}

	transport.mu.Lock()
	folderCalls := transport.callCounts["GetAppFolder"]
	transport.mu.Unlock()
	if folderCalls != 1 {
		t.Errorf("expected exactly 1 GetAppFolder call, got %d", folderCalls)
	}

	// 3. 测试 InvalidateDriveCache
	client.InvalidateDriveCache("valid-user")
	_, _ = client.GenerateAccessToken(ctx, "valid-user")
	transport.mu.Lock()
	generateCallsAfterInvalidate := transport.callCounts["GenerateAccessToken"]
	transport.mu.Unlock()
	if generateCallsAfterInvalidate != 2 {
		t.Errorf("expected 2 GenerateAccessToken calls after invalidate, got %d", generateCallsAfterInvalidate)
	}

	// 4. 测试 unauthorized_client 记忆化
	_, errNoAuth1 := client.GenerateAccessToken(ctx, "no-auth-user")
	if !driveAuthorizationMissing(errNoAuth1) {
		t.Fatalf("expected driveAuthorizationMissing, got %v", errNoAuth1)
	}
	if !client.isDriveAuthMissing("no-auth-user") {
		t.Errorf("expected isDriveAuthMissing to be true")
	}

	transport.mu.Lock()
	noAuthCalls1 := transport.callCounts["GenerateAccessToken"]
	transport.mu.Unlock()

	// 第二次调用应在 0ms 内直接返回错误，不打底层 transport
	_, errNoAuth2 := client.GenerateAccessToken(ctx, "no-auth-user")
	if !driveAuthorizationMissing(errNoAuth2) {
		t.Fatalf("expected driveAuthorizationMissing on second call, got %v", errNoAuth2)
	}
	transport.mu.Lock()
	noAuthCalls2 := transport.callCounts["GenerateAccessToken"]
	transport.mu.Unlock()
	if noAuthCalls2 != noAuthCalls1 {
		t.Errorf("expected no additional RPC call for memoized no-auth, was %d now %d", noAuthCalls1, noAuthCalls2)
	}
}

// TestCanDirectInlineMediaThreshold 测试直接内联通道的判定逻辑
func TestCanDirectInlineMediaThreshold(t *testing.T) {
	// 空任务
	if canDirectInlineMedia(nil) {
		t.Errorf("expected false for nil jobs")
	}

	// 小图片 (1 MiB)
	smallJob := []uploadJob{
		{blob: &Blob{MIME: "image/png", Data: make([]byte, 1<<20)}},
	}
	if !canDirectInlineMedia(smallJob) {
		t.Errorf("expected true for 1 MiB image")
	}

	// 14 MiB 图片
	nearLimitJobs := []uploadJob{
		{blob: &Blob{MIME: "image/jpeg", Data: make([]byte, 14<<20)}},
	}
	if !canDirectInlineMedia(nearLimitJobs) {
		t.Errorf("expected true for 14 MiB image")
	}

	// 16 MiB 图片 (超过 15 MiB 阈值)
	overLimitJobs := []uploadJob{
		{blob: &Blob{MIME: "image/jpeg", Data: make([]byte, 16<<20)}},
	}
	if canDirectInlineMedia(overLimitJobs) {
		t.Errorf("expected false for 16 MiB image")
	}

	// 视频即便很小也不走直接内联 (需要 Drive 播放/时序处理)
	videoJob := []uploadJob{
		{blob: &Blob{MIME: "video/mp4", Data: make([]byte, 100<<10)}},
	}
	if canDirectInlineMedia(videoJob) {
		t.Errorf("expected false for video MIME")
	}
}
