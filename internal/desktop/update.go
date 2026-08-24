package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ReleasesAPI 是项目的最新发布查询端点（公开、无鉴权）
const ReleasesAPI = "https://api.github.com/repos/Heartcoolman/Tg-Down/releases/latest"

const updateHTTPTimeout = 10 * time.Second

// updateBodyLimit 限制 release JSON 的读取量：对端返回超大响应时不该把内存吃光
const updateBodyLimit = 1 << 20

// UpdateCheckDisabledEnv 置为 1/true 时完全关闭更新检查（离线部署与不希望进程外联的场景）
const UpdateCheckDisabledEnv = "TG_DOWN_DESKTOP_NO_UPDATE_CHECK"

// notifiedFile 记录最近一次已弹过通知的版本号，避免每次启动重复打扰
const notifiedFile = "update-notified"

// ErrUpdateCheckDisabled 表示更新检查已被环境变量关闭
var ErrUpdateCheckDisabled = errors.New("更新检查已禁用")

// Release 是一次更新检查的结果
type Release struct {
	Tag     string `json:"tag"`
	URL     string `json:"url"`
	Notes   string `json:"notes,omitempty"`
	Version string `json:"version"` // 规范化后的版本号（无 v 前缀）
}

// UpdateCheckDisabled 报告更新检查是否被关闭
func UpdateCheckDisabled() bool {
	v := strings.TrimSpace(os.Getenv(UpdateCheckDisabledEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// CheckUpdate 查询 GitHub 最新 release；current 为当前版本（dev 视为 0.0.0）。
// 关闭更新检查时返回 ErrUpdateCheckDisabled。
func CheckUpdate(current string) (*Release, error) {
	if UpdateCheckDisabled() {
		return nil, ErrUpdateCheckDisabled
	}
	client := &http.Client{Timeout: updateHTTPTimeout}
	ctx, cancel := context.WithTimeout(context.Background(), updateHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesAPI, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询更新失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询更新失败: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Name       string `json:"name"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, updateBodyLimit)).Decode(&body); err != nil {
		return nil, fmt.Errorf("解析发布信息失败: %w", err)
	}
	if body.TagName == "" {
		return nil, fmt.Errorf("发布信息缺少 tag_name")
	}
	r := &Release{Tag: body.TagName, URL: body.HTMLURL, Notes: body.Name}
	r.Version = strings.TrimPrefix(body.TagName, "v")
	if NewerVersion(r.Version, current) && !body.Prerelease {
		return r, nil
	}
	return nil, nil // 有响应但无需更新：nil, nil
}

// ClaimUpdateNotice 判断某个版本是否还需要自动弹通知，需要则就地记下已通知。
// 返回 false 表示同一版本此前已提示过。
//
// 只作用于启动时的自动检查；托盘里的手动"检查更新"是显式动作，每次都应有回应。
func ClaimUpdateNotice(appDir, version string) bool {
	if version == "" {
		return false
	}
	p := filepath.Join(appDir, notifiedFile)
	if data, err := os.ReadFile(p); err == nil { //nolint:gosec // 应用数据目录内的固定文件名
		if strings.TrimSpace(string(data)) == version {
			return false
		}
	}
	// 写失败只影响去重（下次启动会再提示一遍），不该挡住这一次通知
	_ = os.WriteFile(p, []byte(version), 0o600)
	return true
}

// NewerVersion 判断 candidate 是否严格新于 current；非 semver 形态按段比较，段数不足补零。
// 无法解析时返回 false（宁可不提示，不误报）。
func NewerVersion(candidate, current string) bool {
	c, okc := parseVersion(candidate)
	u, oku := parseVersion(current)
	if !okc || !oku {
		return false
	}
	for i := range max(len(c), len(u)) {
		var cv, uv int64
		if i < len(c) {
			cv = c[i]
		}
		if i < len(u) {
			uv = u[i]
		}
		if cv != uv {
			return cv > uv
		}
	}
	return false
}

func parseVersion(v string) ([]int64, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return []int64{0, 0, 0}, true
	}
	parts := strings.Split(v, ".")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		num := p
		// 剥掉 1.2.0-beta1 这类后缀
		if i := strings.IndexAny(p, "-+"); i >= 0 {
			num = p[:i]
		}
		n, err := strconv.ParseInt(num, 10, 32)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
