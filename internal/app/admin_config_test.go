package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

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
