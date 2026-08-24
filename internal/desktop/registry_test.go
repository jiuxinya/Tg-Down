package desktop

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := OpenRegistry(filepath.Join(t.TempDir(), InstancesFile))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestUpdateValidatesBeforeMutating 非法地址此前会先把 Name 改掉再报错，
// 内存里留下已改名、磁盘上仍是旧名的分叉状态。
func TestUpdateValidatesBeforeMutating(t *testing.T) {
	reg := newTestRegistry(t)
	in, err := reg.Add("NAS", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	newName, badURL := "改过的名字", "ftp://nope"
	if _, err := reg.Update(in.ID, &newName, &badURL, nil); err == nil {
		t.Fatal("非法地址应报错")
	} else if !errors.Is(err, ErrInvalidInstanceURL) {
		t.Fatalf("错误类型 = %v, want ErrInvalidInstanceURL", err)
	}

	got, _ := reg.Get(in.ID)
	if got.Name != "NAS" {
		t.Errorf("校验失败时不应改动内存中的名称，got %q", got.Name)
	}
	// 磁盘与内存必须一致
	reopened, err := OpenRegistry(reg.path)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _ := reopened.Get(in.ID)
	if onDisk.Name != got.Name || onDisk.URL != got.URL {
		t.Errorf("内存 %+v 与磁盘 %+v 不一致", got, onDisk)
	}
}

// TestUpdateRejectsEmptyName 改名成空串此前是静默无操作：界面上什么也没发生，也没有报错
func TestUpdateRejectsEmptyName(t *testing.T) {
	reg := newTestRegistry(t)
	in, err := reg.Add("NAS", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, empty := range []string{"", "   "} {
		if _, err := reg.Update(in.ID, &empty, nil, nil); err == nil {
			t.Errorf("改名为 %q 应报错", empty)
		}
	}
	if got, _ := reg.Get(in.ID); got.Name != "NAS" {
		t.Errorf("名称不应被改动，got %q", got.Name)
	}
}

// TestUpdateNilKeepsFieldsAndEmptyTokenClears token 是唯一允许覆写为空串的字段（清除令牌）
func TestUpdateNilKeepsFieldsAndEmptyTokenClears(t *testing.T) {
	reg := newTestRegistry(t)
	in, err := reg.Add("NAS", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	tok := "secret"
	if _, err := reg.Update(in.ID, nil, nil, &tok); err != nil {
		t.Fatal(err)
	}
	name := "新名字"
	got, err := reg.Update(in.ID, &name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "新名字" || got.URL != "http://127.0.0.1:1/" || got.Token != "secret" {
		t.Fatalf("nil 字段应保持不变: %+v", got)
	}
	empty := ""
	got, err = reg.Update(in.ID, nil, nil, &empty)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "" {
		t.Errorf("空串令牌应清除，got %q", got.Token)
	}
}

// TestAddNormalizesURL handleAdd 的返回值必须能直接用来接着写令牌：
// 服务端会补 scheme 和结尾斜杠，按提交原文回列表里找是找不着的。
func TestAddNormalizesURL(t *testing.T) {
	reg := newTestRegistry(t)
	in, err := reg.Add("", "192.168.1.10:8080")
	if err != nil {
		t.Fatal(err)
	}
	if in.URL != "http://192.168.1.10:8080/" {
		t.Errorf("URL = %q, want http://192.168.1.10:8080/", in.URL)
	}
	if in.ID == "" {
		t.Error("Add 必须返回可用的 ID")
	}
	if in.Name != in.URL {
		t.Errorf("名称为空时应回落到 URL，got %q", in.Name)
	}
	if _, ok := reg.Get(in.ID); !ok {
		t.Error("返回的 ID 必须能在注册表里查到")
	}
}

func TestRegistryErrorsAreDistinguishable(t *testing.T) {
	reg := newTestRegistry(t)
	if _, err := reg.Update("ghost", nil, nil, nil); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("Update 未知 ID = %v, want ErrInstanceNotFound", err)
	}
	if err := reg.Delete("ghost"); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("Delete 未知 ID = %v, want ErrInstanceNotFound", err)
	}
	if err := reg.Select("ghost"); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("Select 未知 ID = %v, want ErrInstanceNotFound", err)
	}
	if _, err := reg.Add("x", "ftp://bad"); !errors.Is(err, ErrInvalidInstanceURL) {
		t.Errorf("Add 非法地址 = %v, want ErrInvalidInstanceURL", err)
	}
}
