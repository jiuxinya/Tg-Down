package desktop

import (
	"errors"
	"testing"
)

// TestUpdateCheckCanBeDisabled 离线部署与不希望进程外联的用户此前没有任何开关可关
func TestUpdateCheckCanBeDisabled(t *testing.T) {
	t.Setenv(UpdateCheckDisabledEnv, "1")
	if !UpdateCheckDisabled() {
		t.Fatal("环境变量置 1 时应视为已关闭")
	}
	if _, err := CheckUpdate("1.0.0"); !errors.Is(err, ErrUpdateCheckDisabled) {
		t.Fatalf("CheckUpdate = %v, want ErrUpdateCheckDisabled", err)
	}

	t.Setenv(UpdateCheckDisabledEnv, "false")
	if UpdateCheckDisabled() {
		t.Error("false 不应关闭更新检查")
	}
}

// TestClaimUpdateNoticeDedups 同一版本每次启动都弹一次通知属于持续打扰
func TestClaimUpdateNoticeDedups(t *testing.T) {
	dir := t.TempDir()
	if !ClaimUpdateNotice(dir, "3.2.0") {
		t.Fatal("首次应允许通知")
	}
	if ClaimUpdateNotice(dir, "3.2.0") {
		t.Error("同一版本不应再次通知")
	}
	if !ClaimUpdateNotice(dir, "3.3.0") {
		t.Error("新版本应重新允许通知")
	}
	if ClaimUpdateNotice(dir, "3.3.0") {
		t.Error("新版本记录后同样应去重")
	}
	if ClaimUpdateNotice(dir, "") {
		t.Error("空版本号不应触发通知")
	}
}
