package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// TestAdminConfig 验证凭据保存、密码保留与管理重启标记
func TestAdminConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	cfg.AdminAuthEnabled = true
	cfg.AdminUsername = "operator"
	cfg.AdminPassword = "test-password"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := aistudio.NewAccountPool(nil, 1)
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx), pool: pool, service: &trackedService{pool: pool}}
	value, err := admin.RuntimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "test-password") || !value.AdminPasswordSet || !value.BuildNativeNonstream {
		t.Fatalf("config=%s", raw)
	}
	value.Proxy = "http://127.0.0.1:1234"
	value.AutoStart = true
	if _, err := admin.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil || saved.AdminPassword != cfg.AdminPassword {
		t.Fatalf("password changed: %v", err)
	}
	if !saved.AutoStart {
		t.Fatal("auto_start was not saved")
	}
	manager := &runtimeManager{activeManagement: cfg}
	password := "new-test-password"
	value.AdminPassword = &password
	value, err = admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	decorated := manager.decorateRuntimeConfig(value, cfg)
	if !decorated.ManagementRestartRequired || decorated.AdminPassword != nil {
		t.Fatalf("restart=%+v", decorated)
	}
	value.AdminPassword = new(string)
	_, err = admin.UpdateRuntimeConfig(ctx, value)
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("empty enabled password error = %v", err)
	}
	value.AdminAuthEnabled = false
	if _, err := admin.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
}

// TestAdminConfigIgnoreThoughtSignatureImmediate 验证修改 IgnoreThoughtSignature 立即生效且不需要重启生成服务
func TestAdminConfigIgnoreThoughtSignatureImmediate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := aistudio.NewAccountPool(nil, 1)
	client := aistudio.NewClient(aistudio.ClientOptions{})
	pooled := aistudio.NewPooledService(pool, client)
	pooled.SetIgnoreThoughtSignature(cfg.IgnoreThoughtSignature)
	service := &trackedService{pool: pool, service: pooled}
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx), pool: pool, service: service}
	manager := &runtimeManager{activeManagement: cfg}

	value, err := admin.RuntimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !value.IgnoreThoughtSignature || !value.StripThoughtSignatures {
		t.Fatalf("default ignore_thought_signature=%v strip_thought_signatures=%v, want both true", value.IgnoreThoughtSignature, value.StripThoughtSignatures)
	}

	value.IgnoreThoughtSignature = false
	updated, err := admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	decorated := manager.decorateRuntimeConfig(updated, cfg)
	if decorated.ServiceRestartRequired {
		t.Fatalf("ServiceRestartRequired = true after changing IgnoreThoughtSignature, want false")
	}
	if decorated.ManagementRestartRequired {
		t.Fatalf("ManagementRestartRequired = true after changing IgnoreThoughtSignature, want false")
	}
	if updated.IgnoreThoughtSignature || updated.StripThoughtSignatures {
		t.Fatalf("updated ignore=%v strip=%v, want both false", updated.IgnoreThoughtSignature, updated.StripThoughtSignatures)
	}
	if pooled.IgnoreThoughtSignature || pooled.StripThoughtSignatures {
		t.Fatalf("pooled ignore=%v strip=%v, want immediate false", pooled.IgnoreThoughtSignature, pooled.StripThoughtSignatures)
	}

	value = updated
	value.IgnoreThoughtSignature = true
	updated, err = admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	decorated = manager.decorateRuntimeConfig(updated, cfg)
	if decorated.ServiceRestartRequired {
		t.Fatalf("ServiceRestartRequired = true after re-enabling IgnoreThoughtSignature, want false")
	}
	if !pooled.IgnoreThoughtSignature || !pooled.StripThoughtSignatures {
		t.Fatalf("pooled ignore=%v strip=%v, want immediate true", pooled.IgnoreThoughtSignature, pooled.StripThoughtSignatures)
	}
}

type stubDriveRPCTransport struct {
	calls atomic.Int32
	fn    func(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error)
}

func (s *stubDriveRPCTransport) Do(ctx context.Context, req aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	s.calls.Add(1)
	return s.fn(ctx, req)
}

// TestRestoreAccountInvalidatesDriveCache 验证手动恢复账户为就绪时会联动清除残留 Drive 缓存且空服务不 panic
func TestRestoreAccountInvalidatesDriveCache(t *testing.T) {
	var nilTracked *trackedService
	nilTracked.InvalidateDriveCache("user@example.com")
	(&trackedService{}).InvalidateDriveCache("user@example.com")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	account := &aistudio.Account{
		ID:          "user@example.com",
		Directory:   dir,
		StoragePath: filepath.Join(dir, "storage-state.json"),
		RuntimePath: filepath.Join(dir, "runtime-state.json"),
		Config:      aistudio.DefaultAccountConfig("user@example.com"),
		Models:      []aistudio.Model{{ID: "gemini-flash-latest"}},
	}
	pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
	store := aistudio.NewAccountStore(dir)
	var deny atomic.Bool
	deny.Store(true)
	transport := &stubDriveRPCTransport{
		fn: func(_ context.Context, req aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			if deny.Load() {
				return nil, &aistudio.RPCError{
					Method:     req.Method,
					StatusCode: http.StatusUnauthorized,
					Code:       16,
					Message:    "OAuth error: unauthorized_client.",
				}
			}
			return &aistudio.RPCResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {aistudio.JSONProtobufContentType}},
				Body:       io.NopCloser(strings.NewReader(`["fresh-drive-token"]`)),
			}, nil
		},
	}
	client := aistudio.NewClient(aistudio.ClientOptions{Transport: transport})
	pooled := aistudio.NewPooledService(pool, client)
	requests := newRequestRegistry(ctx)
	service := newTrackedService(ctx, pooled, pool, requests, nil, time.Minute)
	admin := newRuntimeAdmin(ctx, pool, store, service, requests, nil, nil, config.Default())

	if _, err := client.GenerateAccessToken(ctx, account.ID); err == nil {
		t.Fatal("first GenerateAccessToken succeeded, want unauthorized_client error")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("transport calls after first call = %d, want 1", got)
	}
	// 第二次应命中 driveNoAuth 负缓存，不再请求上游
	if _, err := client.GenerateAccessToken(ctx, account.ID); err == nil {
		t.Fatal("cached GenerateAccessToken succeeded, want cached unauthorized_client error")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("transport calls after cached call = %d, want 1", got)
	}

	deny.Store(false)
	if _, err := admin.SetAccountState(ctx, account.ID, string(aistudio.AccountReady)); err != nil {
		t.Fatalf("SetAccountState(ready) failed: %v", err)
	}

	token, err := client.GenerateAccessToken(ctx, account.ID)
	if err != nil {
		t.Fatalf("GenerateAccessToken after restoreAccount failed: %v", err)
	}
	if token != "fresh-drive-token" {
		t.Fatalf("token = %q, want %q", token, "fresh-drive-token")
	}
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("transport calls after restore = %d, want 2", got)
	}
}
