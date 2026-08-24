package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
)

// LocalInstanceID 是内置本地实例的保留 ID；远程实例不得占用
const LocalInstanceID = "local"

// ErrInstanceNotFound 用于把“实例不存在”与参数非法区分开：前者是 404，后者是 400
var ErrInstanceNotFound = errors.New("实例不存在")

// ErrInvalidInstanceURL 表示地址不是可用的 http(s) 根地址
var ErrInvalidInstanceURL = errors.New("实例地址无效")

// Instance 描述一个可管理的下载实例。本地实例不入注册表，由壳层动态合成。
type Instance struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// instanceFile 是 instances.json 的磁盘结构
type instanceFile struct {
	Instances []*Instance `json:"instances"`
	Selected  string      `json:"selected"`
}

// Registry 管理远程实例列表与当前选中项，串行化落盘。
// Token 仅存于本文件与壳层内存，绝不进入前端。
type Registry struct {
	mu   sync.Mutex
	path string
	data instanceFile
}

// OpenRegistry 从 path 加载注册表；文件不存在时返回空表
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path) //nolint:gosec // path 由应用数据目录拼出，非用户输入
	if os.IsNotExist(err) {
		r.data.Selected = LocalInstanceID
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取实例注册表失败: %w", err)
	}
	if err := json.Unmarshal(data, &r.data); err != nil {
		return nil, fmt.Errorf("解析实例注册表失败: %w", err)
	}
	if r.data.Selected == "" {
		r.data.Selected = LocalInstanceID
	}
	r.normalizeLocked()
	return r, nil
}

// normalizeLocked 清洗：剔除 local 保留 ID、去重、URL 规范化、排序
func (r *Registry) normalizeLocked() {
	seen := make(map[string]bool, len(r.data.Instances))
	kept := r.data.Instances[:0]
	for _, in := range r.data.Instances {
		in.ID = strings.TrimSpace(in.ID)
		in.Name = strings.TrimSpace(in.Name)
		in.URL = NormalizeBaseURL(in.URL)
		if in.ID == "" || in.ID == LocalInstanceID || seen[in.ID] || in.URL == "" {
			continue
		}
		if in.Name == "" {
			in.Name = in.URL
		}
		seen[in.ID] = true
		kept = append(kept, in)
	}
	r.data.Instances = kept
	if r.data.Selected != LocalInstanceID && !seen[r.data.Selected] {
		r.data.Selected = LocalInstanceID
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
}

// saveLocked 原子落盘：先写临时文件并 fsync，再 rename 覆盖。
// 少了 fsync 的话，崩溃/断电后 rename 可能先于数据落到介质，注册表会变成空洞文件，
// 全部远程实例与令牌一起丢失。
func (r *Registry) saveLocked() error {
	data, err := json.MarshalIndent(&r.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path 由应用数据目录拼出
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, r.path)
}

// List 返回全部远程实例（副本）
func (r *Registry) List() []Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Instance, len(r.data.Instances))
	for i, in := range r.data.Instances {
		out[i] = *in
	}
	return out
}

// Get 按 ID 取实例；不存在返回 false
func (r *Registry) Get(id string) (Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, in := range r.data.Instances {
		if in.ID == id {
			return *in, true
		}
	}
	return Instance{}, false
}

// Add 新增实例并返回；name 为空时用 URL 充当名称
func (r *Registry) Add(name, rawURL string) (Instance, error) {
	u := NormalizeBaseURL(rawURL)
	if u == "" {
		return Instance{}, ErrInvalidInstanceURL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, err := newID()
	if err != nil {
		return Instance{}, err
	}
	in := &Instance{ID: id, Name: strings.TrimSpace(name), URL: u}
	if in.Name == "" {
		in.Name = u
	}
	before := r.data.Instances
	r.data.Instances = append(append([]*Instance{}, before...), in)
	r.normalizeLocked()
	if err := r.saveLocked(); err != nil {
		r.data.Instances = before
		return Instance{}, err
	}
	return *in, nil
}

// Update 就地修改实例。三个可选字段一律 nil 表示保持不变、非 nil 表示覆写：
// name 覆写为空串是明确的错误（此前空串与“不修改”不可区分，改名成空是静默无操作），
// token 覆写为空串则是合法的“清除令牌”。
//
// 全部校验在改动内存之前完成，落盘失败时回滚：否则一次非法地址或一次写盘失败
// 就会让内存与磁盘分叉，界面显示的值与重启后的值不一致。
func (r *Registry) Update(id string, name, rawURL, token *string) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var target *Instance
	for _, in := range r.data.Instances {
		if in.ID == id {
			target = in
			break
		}
	}
	if target == nil {
		return Instance{}, fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
	}

	newName := target.Name
	if name != nil {
		if newName = strings.TrimSpace(*name); newName == "" {
			return Instance{}, errors.New("实例名称不能为空")
		}
	}
	newURL := target.URL
	if rawURL != nil {
		if newURL = NormalizeBaseURL(*rawURL); newURL == "" {
			return Instance{}, ErrInvalidInstanceURL
		}
	}
	newToken := target.Token
	if token != nil {
		newToken = strings.TrimSpace(*token)
	}

	before := *target
	target.Name, target.URL, target.Token = newName, newURL, newToken
	r.normalizeLocked()
	if err := r.saveLocked(); err != nil {
		*target = before
		r.normalizeLocked()
		return Instance{}, err
	}
	return *target, nil
}

// Delete 删除实例；若删除的是选中项则回落本地
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]*Instance, 0, len(r.data.Instances))
	found := false
	for _, in := range r.data.Instances {
		if in.ID == id {
			found = true
			continue
		}
		kept = append(kept, in)
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
	}
	beforeList, beforeSel := r.data.Instances, r.data.Selected
	r.data.Instances = kept
	if r.data.Selected == id {
		r.data.Selected = LocalInstanceID
	}
	r.normalizeLocked()
	if err := r.saveLocked(); err != nil {
		r.data.Instances, r.data.Selected = beforeList, beforeSel
		return err
	}
	return nil
}

// Selected 返回当前选中的实例 ID（local 或远程 ID）
func (r *Registry) Selected() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data.Selected
}

// Select 切换选中项；id 必须为 local 或已存在的远程实例
func (r *Registry) Select(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != LocalInstanceID {
		found := false
		for _, in := range r.data.Instances {
			if in.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
		}
	}
	before := r.data.Selected
	r.data.Selected = id
	if err := r.saveLocked(); err != nil {
		r.data.Selected = before
		return err
	}
	return nil
}

// newID 生成 8 字节随机十六进制 ID
func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成实例 ID 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NormalizeBaseURL 规范化实例地址：去空白与结尾斜杠，校验 scheme/host。
// 返回空串表示输入不可用。
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if u.Path != "" && u.Path != "/" {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/") + "/"
}
