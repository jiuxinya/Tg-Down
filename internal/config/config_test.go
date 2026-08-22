package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// 测试常量
const (
	TestAPIID   = 12345
	TestAPIHash = "0123456789abcdef0123456789abcdef"
	TestPhone   = "+1234567890"
)

func TestLoadConfig(t *testing.T) {
	// 设置测试环境变量
	if err := os.Setenv("API_ID", "12345"); err != nil {
		t.Fatalf("Failed to set API_ID: %v", err)
	}
	if err := os.Setenv("API_HASH", TestAPIHash); err != nil {
		t.Fatalf("Failed to set API_HASH: %v", err)
	}
	if err := os.Setenv("PHONE", TestPhone); err != nil {
		t.Fatalf("Failed to set PHONE: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("API_ID"); err != nil {
			t.Errorf("Failed to unset API_ID: %v", err)
		}
		if err := os.Unsetenv("API_HASH"); err != nil {
			t.Errorf("Failed to unset API_HASH: %v", err)
		}
		if err := os.Unsetenv("PHONE"); err != nil {
			t.Errorf("Failed to unset PHONE: %v", err)
		}
	}()

	config, err := LoadConfig()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if config.API.ID != TestAPIID {
		t.Errorf("Expected API ID %d, got %d", TestAPIID, config.API.ID)
	}
	if config.API.Hash != TestAPIHash {
		t.Errorf("Expected API Hash '%s', got %s", TestAPIHash, config.API.Hash)
	}
	if config.API.Phone != TestPhone {
		t.Errorf("Expected Phone '%s', got %s", TestPhone, config.API.Phone)
	}
}

func TestLoadConfigWithDefaults(t *testing.T) {
	if err := os.Setenv("API_ID", "12345"); err != nil {
		t.Fatalf("Failed to set API_ID: %v", err)
	}
	if err := os.Setenv("API_HASH", TestAPIHash); err != nil {
		t.Fatalf("Failed to set API_HASH: %v", err)
	}
	if err := os.Setenv("PHONE", TestPhone); err != nil {
		t.Fatalf("Failed to set PHONE: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("API_ID"); err != nil {
			t.Errorf("Failed to unset API_ID: %v", err)
		}
		if err := os.Unsetenv("API_HASH"); err != nil {
			t.Errorf("Failed to unset API_HASH: %v", err)
		}
		if err := os.Unsetenv("PHONE"); err != nil {
			t.Errorf("Failed to unset PHONE: %v", err)
		}
	}()

	config, err := LoadConfig()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// 检查默认值
	if config.Download.Path != DefaultDownloadPath {
		t.Errorf("Expected default download path '%s', got %s", DefaultDownloadPath, config.Download.Path)
	}
	if config.Download.MaxConcurrent != DefaultMaxConcurrent {
		t.Errorf("Expected default max concurrent %d, got %d", DefaultMaxConcurrent, config.Download.MaxConcurrent)
	}
	if config.Download.BatchSize != DefaultBatchSize {
		t.Errorf("Expected default batch size %d, got %d", DefaultBatchSize, config.Download.BatchSize)
	}
	if config.Log.Level != DefaultLogLevel {
		t.Errorf("Expected default log level '%s', got %s", DefaultLogLevel, config.Log.Level)
	}
	if config.Session.Dir != DefaultSessionDir {
		t.Errorf("Expected default session dir '%s', got %s", DefaultSessionDir, config.Session.Dir)
	}
	if config.Queue.MaxConcurrentTasks != DefaultMaxConcurrentTasks {
		t.Errorf("Expected default max concurrent tasks %d, got %d", DefaultMaxConcurrentTasks, config.Queue.MaxConcurrentTasks)
	}
	if config.Store.Path != DefaultStorePath {
		t.Errorf("Expected default store path '%s', got %s", DefaultStorePath, config.Store.Path)
	}
}

func TestLoadConfigFallsBackForInvalidConcurrency(t *testing.T) {
	t.Setenv("API_ID", "12345")
	t.Setenv("API_HASH", TestAPIHash)
	t.Setenv("PHONE", TestPhone)
	t.Setenv("MAX_CONCURRENT_DOWNLOADS", "-1")
	t.Setenv("MAX_CONCURRENT_TASKS", "-2")

	config, err := LoadConfig()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if config.Download.MaxConcurrent != DefaultMaxConcurrent {
		t.Errorf("Expected fallback max concurrent %d, got %d", DefaultMaxConcurrent, config.Download.MaxConcurrent)
	}
	if config.Queue.MaxConcurrentTasks != DefaultMaxConcurrentTasks {
		t.Errorf("Expected fallback max concurrent tasks %d, got %d", DefaultMaxConcurrentTasks, config.Queue.MaxConcurrentTasks)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	// 清除所有相关环境变量
	if err := os.Unsetenv("API_ID"); err != nil {
		t.Errorf("Failed to unset API_ID: %v", err)
	}
	if err := os.Unsetenv("API_HASH"); err != nil {
		t.Errorf("Failed to unset API_HASH: %v", err)
	}
	if err := os.Unsetenv("PHONE"); err != nil {
		t.Errorf("Failed to unset PHONE: %v", err)
	}

	_, err := LoadConfig()
	if err == nil {
		t.Error("Expected error for missing required config, but got none")
	}
}

func TestSaveConfig(t *testing.T) {
	t.Setenv("TG_DOWN_NO_CONFIG_WRITE", "")
	config := &Config{
		API: APIConfig{
			ID:    TestAPIID,
			Hash:  TestAPIHash,
			Phone: TestPhone,
		},
		Download: DownloadConfig{
			Path:          DefaultDownloadPath,
			MaxConcurrent: DefaultMaxConcurrent,
			BatchSize:     DefaultBatchSize,
		},
		Log: LogConfig{
			Level: DefaultLogLevel,
		},
		Session: SessionConfig{
			Dir: DefaultSessionDir,
		},
	}

	// 模拟用户从示例复制出的宽权限文件，保存后必须收紧到 0600。
	tempFile := t.TempDir() + "/test_config.yaml"
	if err := os.WriteFile(tempFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := config.SaveConfig(tempFile)
	if err != nil {
		t.Errorf("Failed to save config: %v", err)
	}

	// 验证文件是否存在
	if _, err := os.Stat(tempFile); os.IsNotExist(err) {
		t.Error("Config file was not created")
	}
	info, err := os.Stat(tempFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != FilePermission {
		if runtime.GOOS == "windows" {
			t.Skip("Windows 不提供 Unix 权限位语义")
		}
		t.Errorf("配置文件权限 = %o, want %o", got, FilePermission)
	}
}

func TestLoadConfigTightensExistingFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不提供 Unix 权限位语义")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfigForWeb(); err != nil {
		t.Fatalf("LoadConfigForWeb() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != FilePermission {
		t.Fatalf("加载后 config.yaml 权限 = %o, want %o", got, FilePermission)
	}
}

func TestLoadConfigTightensDotEnvPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不提供 Unix 权限位语义")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("LOG_LEVEL=debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfigForWeb(); err != nil {
		t.Fatalf("LoadConfigForWeb() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != FilePermission {
		t.Fatalf("加载后 .env 权限 = %o, want %o", got, FilePermission)
	}
}

func TestSaveConfigConcurrentWritesRemainValid(t *testing.T) {
	t.Setenv("TG_DOWN_NO_CONFIG_WRITE", "")
	filename := filepath.Join(t.TempDir(), "config.yaml")

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 1; i <= writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cfg := &Config{API: APIConfig{ID: id, Hash: TestAPIHash, Phone: TestPhone}}
			errs <- cfg.SaveConfig(filename)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发保存失败: %v", err)
		}
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("最终配置不是完整 YAML: %v\n%s", err, data)
	}
	if got.API.ID < 1 || got.API.ID > writers {
		t.Fatalf("最终 API ID = %d, want 1..%d", got.API.ID, writers)
	}
}

func TestAPICredentialsRejectInvalidID(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("API_HASH", TestAPIHash)
	t.Setenv("PHONE", TestPhone)

	for _, id := range []string{"-1", "2147483648"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("API_ID", id)
			cfg, err := LoadConfigForWeb()
			if err != nil {
				t.Fatalf("LoadConfigForWeb() error = %v", err)
			}
			if cfg.HasAPICredentials() {
				t.Fatalf("HasAPICredentials() = true for API_ID %s", id)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig() accepted API_ID %s", id)
			}
		})
	}
}

func TestLoadConfigRejectsMalformedEnvironmentValues(t *testing.T) {
	t.Chdir(t.TempDir())

	for _, tt := range []struct {
		name  string
		value string
	}{
		{name: "API_ID", value: "12x"},
		{name: "MAX_CONCURRENT_DOWNLOADS", value: "many"},
		{name: "TARGET_CHAT_ID", value: "not-a-chat"},
		{name: "MAX_RETRIES", value: "three"},
		{name: "AUTO_RETRY", value: "sometimes"},
		{name: "SAVE_METADATA", value: "perhaps"},
		{name: "NOTIFY_TELEGRAM_SELF", value: "enabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.name, tt.value)
			_, err := LoadConfigForWeb()
			if err == nil {
				t.Fatalf("LoadConfigForWeb() accepted %s=%q", tt.name, tt.value)
			}
			if !strings.Contains(err.Error(), tt.name) {
				t.Fatalf("error %q does not identify %s", err, tt.name)
			}
		})
	}
}

func TestLoadConfigReportsMalformedDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("API_ID='unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfigForWeb()
	if err == nil {
		t.Fatal("LoadConfigForWeb() accepted malformed .env")
	}
	if !strings.Contains(err.Error(), ".env") {
		t.Fatalf("error does not identify .env: %v", err)
	}
}

func TestLoadSessionDirIgnoresUnrelatedInvalidValues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	configPath := filepath.Join(dir, "config.yaml")
	data := []byte("api:\n  id: not-a-number\nsession:\n  dir: ./from-yaml\n")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("API_ID", "also-not-a-number")
	t.Setenv("MAX_RETRIES", "many")

	got, err := LoadSessionDir()
	if err != nil {
		t.Fatalf("LoadSessionDir() error = %v", err)
	}
	if got != "./from-yaml" {
		t.Fatalf("LoadSessionDir() = %q, want %q", got, "./from-yaml")
	}

	t.Setenv("SESSION_DIR", "./from-env")
	got, err = LoadSessionDir()
	if err != nil {
		t.Fatalf("LoadSessionDir() with env error = %v", err)
	}
	if got != "./from-env" {
		t.Fatalf("LoadSessionDir() with env = %q, want %q", got, "./from-env")
	}
}

func TestAPICredentialValidators(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "valid", valid: IsValidAPIHash(TestAPIHash) && IsValidPhone(TestPhone)},
		{name: "short hash", valid: IsValidAPIHash("abc")},
		{name: "non hex hash", valid: IsValidAPIHash("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")},
		{name: "phone without plus", valid: IsValidPhone("1234567890")},
		{name: "phone starting with zero", valid: IsValidPhone("+01234567")},
		{name: "phone with unicode digits", valid: IsValidPhone("+一二三四五六七")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.name == "valid"
			if tt.valid != want {
				t.Fatalf("valid = %v, want %v", tt.valid, want)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidHashAndPhone(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("API_ID", "12345")

	tests := []struct {
		name  string
		hash  string
		phone string
	}{
		{name: "invalid hash", hash: "not-a-telegram-api-hash", phone: TestPhone},
		{name: "invalid phone", hash: TestAPIHash, phone: "1234567890"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("API_HASH", tt.hash)
			t.Setenv("PHONE", tt.phone)
			cfg, err := LoadConfigForWeb()
			if err != nil {
				t.Fatalf("LoadConfigForWeb() error = %v", err)
			}
			if cfg.HasAPICredentials() {
				t.Fatal("HasAPICredentials() accepted invalid credentials")
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() accepted invalid credentials")
			}
		})
	}
}

func TestLoadConfigReadsTelegramProxy(t *testing.T) {
	t.Setenv("API_ID", "12345")
	t.Setenv("API_HASH", TestAPIHash)
	t.Setenv("PHONE", TestPhone)

	dir := t.TempDir()
	t.Chdir(dir)
	data := []byte("api:\n  id: 12345\ntelegram:\n  proxy: \"socks5://127.0.0.1:1080\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigForWeb()
	if err != nil {
		t.Fatalf("LoadConfigForWeb() error = %v", err)
	}
	if cfg.Telegram.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("Telegram.Proxy = %q, want %q", cfg.Telegram.Proxy, "socks5://127.0.0.1:1080")
	}

	// SaveConfig 往返后代理配置不能丢（Web 端保存凭据时会整体重写 config.yaml）
	if err := cfg.SaveConfig(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	roundTripped, err := LoadConfigForWeb()
	if err != nil {
		t.Fatalf("round-trip LoadConfigForWeb() error = %v", err)
	}
	if roundTripped.Telegram.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("SaveConfig 往返后 Telegram.Proxy = %q, 配置不应丢失", roundTripped.Telegram.Proxy)
	}
}

func TestTelegramProxyDefaultsToEmpty(t *testing.T) {
	t.Setenv("API_ID", "12345")
	t.Setenv("API_HASH", TestAPIHash)
	t.Setenv("PHONE", TestPhone)
	t.Chdir(t.TempDir())

	cfg, err := LoadConfigForWeb()
	if err != nil {
		t.Fatalf("LoadConfigForWeb() error = %v", err)
	}
	if cfg.Telegram.Proxy != "" {
		t.Fatalf("Telegram.Proxy 默认应为空（直连），得到 %q", cfg.Telegram.Proxy)
	}
}
