package config

import "testing"

// TestValidateProxyCredentials 验证代理 URL 接受账号密码并拒绝空用户名
func TestValidateProxyCredentials(t *testing.T) {
	for _, value := range []string{"http://user:secret@127.0.0.1:8080", "socks5://user:secret@127.0.0.1:1080"} {
		if err := ValidateProxy(value); err != nil {
			t.Errorf("ValidateProxy(%q) = %v", value, err)
		}
	}
	if err := ValidateProxy("http://:secret@127.0.0.1:8080"); err == nil {
		t.Error("空用户名被接受")
	}
}

func TestThoughtSignatureConfigSync(t *testing.T) {
	def := Default()
	if !def.IgnoreThoughtSignature || !def.StripThoughtSignatures {
		t.Fatalf("expected both thought signature flags true by default, got ignore=%v strip=%v", def.IgnoreThoughtSignature, def.StripThoughtSignatures)
	}

	path := writeTestEnv(t, "IGNORE_THOUGHT_SIGNATURE=false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IgnoreThoughtSignature || cfg.StripThoughtSignatures {
		t.Fatalf("expected IGNORE_THOUGHT_SIGNATURE=false to sync both to false, got ignore=%v strip=%v", cfg.IgnoreThoughtSignature, cfg.StripThoughtSignatures)
	}

	path2 := writeTestEnv(t, "STRIP_THOUGHT_SIGNATURES=false\n")
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.IgnoreThoughtSignature || cfg2.StripThoughtSignatures {
		t.Fatalf("expected STRIP_THOUGHT_SIGNATURES=false to sync both to false, got ignore=%v strip=%v", cfg2.IgnoreThoughtSignature, cfg2.StripThoughtSignatures)
	}
}
