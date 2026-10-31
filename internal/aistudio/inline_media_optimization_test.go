package aistudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStickyRoutingForInlineMedia 测试内联附件提取、带内联缓存命中加权的调度排序及端到端粘性路由
func TestStickyRoutingForInlineMedia(t *testing.T) {
	img1Data := []byte("image-data-1")
	img2Data := []byte("image-data-2")
	h1 := sha256.Sum256(img1Data)
	h2 := sha256.Sum256(img2Data)
	_ = h1
	_ = h2

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

	// 测试调度池粘性排序与端到端获取
	now := time.Now().UTC()
	model := Model{ID: "gemini-2.5-flash", Methods: []string{"generateContent"}}
	acc1 := &Account{ID: "acc-1", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, runtime: accountRuntimeState{Resources: make(map[string]ResourceBinding)}}
	acc2 := &Account{ID: "acc-2", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, runtime: accountRuntimeState{Resources: make(map[string]ResourceBinding)}}
	acc3 := &Account{ID: "acc-3", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, runtime: accountRuntimeState{Resources: make(map[string]ResourceBinding)}}

	// acc2 缓存了 img1 (1 命中)
	acc2.runtime.Resources["drive-img-1"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now,
	}
	// acc3 缓存了 img1 和 img2 (2 命中)
	acc3.runtime.Resources["drive-img-1-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now,
	}
	acc3.runtime.Resources["drive-img-2-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[1].MIME, SHA256: keys[1].SHA256, DisplayName: keys[1].Name, UsedAt: now,
	}

	pool := NewAccountPool([]*Account{acc1, acc2, acc3}, 1)
	pool.SetRoutingStrategy("round-robin")

	selection := AccountSelection{
		ModelID:         "gemini-2.5-flash",
		Method:          "generateContent",
		InlineMediaKeys: keys,
	}

	// 1. 验证 selectionIndicesLocked
	indices, err := pool.selectionIndicesLocked(selection)
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

	// 2. 验证 channelCandidatesLocked: 无论 lastPicked 处于何种状态，acc3 始终排在候选首位
	pool.lastPicked["gemini-2.5-flash"] = "acc-3"
	candidates := pool.channelCandidatesLocked(indices, selection)
	if len(candidates) != 3 || pool.accounts[candidates[0].index].ID != "acc-3" {
		t.Fatalf("expected channelCandidatesLocked to have acc-3 first even when lastPicked is acc-3, got %s", pool.accounts[candidates[0].index].ID)
	}

	// 3. 端到端验证 pool.AcquireFor: 连续多次真实请求必须 100% 粘在命中数最高的 acc-3 上，不发生轮询漂移
	ctx := context.Background()
	acquireAccount := func(sel AccountSelection) string {
		lease, err := pool.AcquireFor(ctx, sel)
		if err != nil {
			t.Fatalf("AcquireFor failed for selection %+v: %v", sel, err)
		}
		accountID := lease.Account().ID
		if err := lease.Release(); err != nil {
			t.Fatalf("lease.Release failed: %v", err)
		}
		return accountID
	}

	for i := 0; i < 6; i++ {
		picked := acquireAccount(selection)
		if picked != "acc-3" {
			t.Fatalf("request %d: expected sticky account acc-3, got %s", i, picked)
		}
	}

	// 4. 中间穿插其它不带附件的请求改变 lastPicked，验证带附件的请求依然 100% 粘在 acc-3 上
	normalSelection := AccountSelection{
		ModelID: "gemini-2.5-flash",
		Method:  "generateContent",
	}
	for i := 0; i < 6; i++ {
		normalPicked := acquireAccount(normalSelection)
		stickyPicked := acquireAccount(selection)
		if stickyPicked != "acc-3" {
			t.Fatalf("interspersed request %d: expected sticky acc-3, got %s (normal request picked %s)", i, stickyPicked, normalPicked)
		}
	}

	// 5. 测试平局场景（两个账号拥有相同数量的缓存，如各命中 1 个附件）
	delete(acc3.runtime.Resources, "drive-img-2-b")
	// 此时 acc2 缓存了 img1 (1 命中)，acc3 缓存了 img1 (1 命中)，acc1 (0 命中)
	// 验证 selectionIndicesLocked 在命中数平局时按确定性账号 ID 字典序排列 (acc-2 < acc-3)
	tiedIndices, err := pool.selectionIndicesLocked(selection)
	if err != nil {
		t.Fatalf("selectionIndicesLocked for tied cache failed: %v", err)
	}
	if pool.accounts[tiedIndices[0]].ID != "acc-2" || pool.accounts[tiedIndices[1]].ID != "acc-3" {
		t.Fatalf("expected [acc-2, acc-3] for tied cache hits, got [%s, %s]", pool.accounts[tiedIndices[0]].ID, pool.accounts[tiedIndices[1]].ID)
	}

	// 连续真实请求并在中间穿插普通请求，验证始终固定选择确定性的 acc-2，不会在 acc-2 与 acc-3 之间来回跳动
	for i := 0; i < 6; i++ {
		_ = acquireAccount(normalSelection)
		tiedPicked := acquireAccount(selection)
		if tiedPicked != "acc-2" {
			t.Fatalf("tied request %d: expected deterministic acc-2, got %s", i, tiedPicked)
		}
	}

	// 6. 验证未命中的新附件请求依然正常按 round-robin 轮询
	newKeys := []InlineMediaKey{
		{MIME: "image/png", SHA256: "new-unseen-sha256", Name: "new.png"},
	}
	newSelection := AccountSelection{
		ModelID:         "gemini-2.5-flash",
		Method:          "generateContent",
		InlineMediaKeys: newKeys,
	}
	var roundRobinPicks []string
	for i := 0; i < 6; i++ {
		roundRobinPicks = append(roundRobinPicks, acquireAccount(newSelection))
	}
	for i := 0; i < len(roundRobinPicks)-1; i++ {
		if roundRobinPicks[i] == roundRobinPicks[i+1] {
			t.Fatalf("new keys should round-robin, but got consecutive %s at %d and %d: %v", roundRobinPicks[i], i, i+1, roundRobinPicks)
		}
	}
	seen := make(map[string]bool)
	for _, p := range roundRobinPicks {
		seen[p] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected all 3 accounts to be visited in round-robin for new keys, got %v", roundRobinPicks)
	}

	// 7. 测试过期时间: 超过 inlineMediaIdle 的缓存不应算作命中
	acc2.runtime.Resources["drive-img-1"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now.Add(-2 * time.Hour),
	}
	acc3.runtime.Resources["drive-img-1-b"] = ResourceBinding{
		Kind: "inline-media", MIME: keys[0].MIME, SHA256: keys[0].SHA256, DisplayName: keys[0].Name, UsedAt: now.Add(-2 * time.Hour),
	}
	hitsAcc2 := pool.inlineMediaHitsLocked(acc2, keys, now)
	hitsAcc3 := pool.inlineMediaHitsLocked(acc3, keys, now)
	if hitsAcc2 != 0 || hitsAcc3 != 0 {
		t.Errorf("expected 0 hits for expired items, got acc2=%d acc3=%d", hitsAcc2, hitsAcc3)
	}
}

// TestExtractInlineMediaKeysAndStickyRouting 测试内联附件提取与带内联缓存命中加权的轮询粘性调度（兼容旧名）
func TestExtractInlineMediaKeysAndStickyRouting(t *testing.T) {
	TestStickyRoutingForInlineMedia(t)
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

	// 5. 测试 driveNoAuth TTL 过期后自动重新探测
	client.driveCacheMu.Lock()
	client.driveNoAuth["no-auth-user"] = time.Now().Add(-time.Second)
	client.driveCacheMu.Unlock()
	if client.isDriveAuthMissing("no-auth-user") {
		t.Errorf("expected isDriveAuthMissing to be false after TTL expiry")
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

// TestFormatBytes 验证字节数自适应格式化逻辑
func TestFormatBytes(t *testing.T) {
	testCases := []struct {
		bytes    int64
		expected string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1KB"},
		{1536, "1.50KB"},
		{12950448, "12.35MB"},
		{15 * 1024 * 1024, "15MB"},
		{1024 * 1024 * 1024, "1GB"},
		{1099511627776, "1TB"},
		{-512, "-512B"},
		{-1024, "-1KB"},
		{-1536, "-1.50KB"},
		{-12950448, "-12.35MB"},
		{-15 * 1024 * 1024, "-15MB"},
		{-1024 * 1024 * 1024, "-1GB"},
		{math.MinInt64, "-8388608TB"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%d", tc.bytes), func(t *testing.T) {
			if got := FormatBytes(tc.bytes); got != tc.expected {
				t.Errorf("FormatBytes(%d) = %q, expected %q", tc.bytes, got, tc.expected)
			}
		})
	}
}
