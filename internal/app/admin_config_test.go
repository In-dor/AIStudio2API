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

type stubStateService struct {
	managedService
	state string
}

func (s *stubStateService) State() string { return s.state }

// TestAdminConfig 验证凭据保存、密码保留、热加载键不触发重启与仅必要键触发重启标记
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
	requests := newRequestRegistry()
	admin := &runtimeAdmin{configPath: path, requests: requests, pool: pool, service: &trackedService{pool: pool}}
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
	if value.AccountSwitchInterval != config.AccountSwitchIntervalOff || value.MaxQuotaSwitches != config.MaxQuotaSwitchesOff {
		t.Fatalf("default switch policy = interval:%q quota:%q", value.AccountSwitchInterval, value.MaxQuotaSwitches)
	}
	value.Proxy = "http://127.0.0.1:1234"
	value.AutoStart = true
	value.AccountSwitchInterval = "5"
	value.MaxQuotaSwitches = "1"
	if _, err := admin.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil || saved.AdminPassword != cfg.AdminPassword {
		t.Fatalf("password changed: %v", err)
	}
	if !saved.AutoStart || saved.AccountSwitchInterval != "5" || saved.MaxQuotaSwitches != "1" {
		t.Fatalf("saved fields mismatch: auto_start=%v interval=%q quota=%q", saved.AutoStart, saved.AccountSwitchInterval, saved.MaxQuotaSwitches)
	}
	stateSvc := &stubStateService{state: "RUNNING"}
	requests.start(aistudio.GenerateRequest{ID: "req-active", Model: "gemini-flash-latest"}, func() {})
	manager := &runtimeManager{
		activeManagement: cfg,
		requests:         requests,
		current:          &runtimeGeneration{service: stateSvc, config: cfg},
	}
	password := "new-test-password"
	value.AdminPassword = &password
	value.APIKey = "new-hot-api-key"
	value.BuildNativeNonstream = false
	value.YouTubeLinks = false
	value.StripThoughtSignatures = true
	value.ContinuationRewrite = false
	value.ContinuationPrompt = "custom prompt"
	value.RequestTimeout = "90s"
	value.InitTimeout = "45s"
	value.FirstEventTimeout = "30"
	value.FirstEventHedge = "10"
	value.FirstEventHedgeModels = "gemini-2.5-pro"
	value.UpstreamChannels = []string{"playground", "build"}
	value.DiscoverHiddenModels = true
	value, err = admin.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	decorated := manager.decorateRuntimeConfig(value, cfg)
	if decorated.ManagementRestartRequired || decorated.ServiceRestartRequired || decorated.AdminPassword != nil || decorated.ActiveRequests != 1 {
		t.Fatalf("hot reload fields should not require restart and should report active_requests=1: %+v", decorated)
	}
	// 仅修改监听地址或预览监听地址才触发管理进程重启
	value.ListenAddr = "127.0.0.1:2999"
	decorated = manager.decorateRuntimeConfig(value, cfg)
	if !decorated.ManagementRestartRequired {
		t.Fatalf("listen_addr change should require management restart: %+v", decorated)
	}
	value.ListenAddr = cfg.ListenAddr
	value.PreviewListenAddr = "127.0.0.1:3050"
	decorated = manager.decorateRuntimeConfig(value, cfg)
	if !decorated.ManagementRestartRequired {
		t.Fatalf("preview_listen_addr change should require management restart: %+v", decorated)
	}
	value.PreviewListenAddr = cfg.PreviewListenAddr
	// 修改 WAA_BACKEND / AISTUDIO_AUTH_STATES 仅在生成服务运行时触发重启标记，已停止时不触发
	value.WAABackend = config.WAABackendCamoufox
	stateSvc.state = "STOPPED"
	if got := manager.decorateRuntimeConfig(value, cfg); got.ServiceRestartRequired {
		t.Fatalf("stopped service should not require restart: %+v", got)
	}
	stateSvc.state = "RUNNING"
	if got := manager.decorateRuntimeConfig(value, cfg); !got.ServiceRestartRequired {
		t.Fatalf("running service should require restart on WAA_BACKEND change: %+v", got)
	}
	value.WAABackend = cfg.WAABackend
	value.AuthStates = "auth-other"
	if got := manager.decorateRuntimeConfig(value, cfg); !got.ServiceRestartRequired {
		t.Fatalf("running service should require restart on AISTUDIO_AUTH_STATES change: %+v", got)
	}
	value.AuthStates = cfg.AuthStates
	// 非法换号间隔与额度换号上限返回 400
	badInterval := value
	badInterval.AccountSwitchInterval = "0"
	if _, err := admin.UpdateRuntimeConfig(ctx, badInterval); err == nil {
		t.Fatal("ACCOUNT_SWITCH_INTERVAL=0 should be rejected")
	}
	badQuota := value
	badQuota.MaxQuotaSwitches = "-1"
	if _, err := admin.UpdateRuntimeConfig(ctx, badQuota); err == nil {
		t.Fatal("MAX_QUOTA_SWITCHES=-1 should be rejected")
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
	service := &trackedService{pool: pool, pooled: pooled, service: pooled}
	requests := newRequestRegistry()
	defer requests.close()
	admin := &runtimeAdmin{configPath: path, requests: requests, pool: pool, service: service}
	manager := &runtimeManager{
		activeManagement: cfg,
		current:          &runtimeGeneration{service: &stubStateService{state: "RUNNING"}, config: cfg},
	}

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
	if pooled.IgnoreThoughtSignatureEnabled() {
		t.Fatalf("pooled IgnoreThoughtSignatureEnabled() = true, want immediate false")
	}
	if saved, err := config.Load(path); err != nil {
		t.Fatal(err)
	} else {
		service.ApplyHotConfig(saved)
		if pooled.IgnoreThoughtSignatureEnabled() {
			t.Fatalf("pooled IgnoreThoughtSignatureEnabled() = true after ApplyHotConfig(false), want false")
		}
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
	if !pooled.IgnoreThoughtSignatureEnabled() {
		t.Fatalf("pooled IgnoreThoughtSignatureEnabled() = false, want immediate true")
	}
	if saved, err := config.Load(path); err != nil {
		t.Fatal(err)
	} else {
		service.ApplyHotConfig(saved)
		if !pooled.IgnoreThoughtSignatureEnabled() {
			t.Fatalf("pooled IgnoreThoughtSignatureEnabled() = false after ApplyHotConfig(true), want true")
		}
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
	requests := newRequestRegistry()
	defer requests.close()
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
