package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"tg-down/internal/web"
)

func goOS() string   { return runtime.GOOS }
func goArch() string { return runtime.GOARCH }

// webUI 返回内嵌 SPA 处理器（与 Web 管理台同一份构建产物）
func webUI() http.Handler { return web.UIHandler() }

// infoDTO 是 GET /desktop/api/info 的响应：前端据此判断桌面环境并展示壳层信息
type infoDTO struct {
	Version   string `json:"version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	AppDir    string `json:"app_dir"`
	Autostart bool   `json:"autostart"`
	Local     string `json:"local"` // 本地引擎根地址（诊断用）
}

func (s *Shell) handleInfo(w http.ResponseWriter, _ *http.Request) {
	enabled, _ := s.autostart.Enabled()
	writeJSON(w, infoDTO{
		Version:   s.version,
		GOOS:      goOS(),
		GOARCH:    goArch(),
		AppDir:    s.appDir,
		Autostart: enabled,
		Local:     s.engineBase,
	})
}

func (s *Shell) handleList(w http.ResponseWriter, _ *http.Request) {
	list := s.reg.List()
	dtos := make([]instanceDTO, len(list))
	for i, in := range list {
		dtos[i] = instanceDTO{ID: in.ID, Name: in.Name, URL: in.URL, HasToken: in.Token != ""}
	}
	writeJSON(w, map[string]any{"selected": s.reg.Selected(), "instances": dtos})
}

// instanceDTO 不携带 Token 原文；前端只见 has_token
type instanceDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	HasToken bool   `json:"has_token"`
}

func (s *Shell) handleAdd(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}](w, r)
	if !ok {
		return
	}
	in, err := s.reg.Add(body.Name, body.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, instanceDTO{ID: in.ID, Name: in.Name, URL: in.URL, HasToken: in.Token != ""})
}

func (s *Shell) handleUpdate(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[struct {
		Name  *string `json:"name"`
		URL   *string `json:"url"`
		Token *string `json:"token"`
	}](w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	in, err := s.reg.Update(id, deref(body.Name), deref(body.URL), body.Token)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, instanceDTO{ID: in.ID, Name: in.Name, URL: in.URL, HasToken: in.Token != ""})
}

func (s *Shell) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Shell) handleSelect(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[struct {
		ID string `json:"id"`
	}](w, r)
	if !ok {
		return
	}
	if err := s.reg.Select(body.ID); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "selected": body.ID})
}

// handleTest 对实例发一次带鉴权的 /api/state 探测，返回连通性与版本信息
func (s *Shell) handleTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var result struct {
		OK      bool   `json:"ok"`
		Version string `json:"version,omitempty"`
		State   string `json:"state,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	state, err := probeInstance(s.reg, id, r.Context())
	if err != nil {
		result.Error = err.Error()
	} else {
		result.OK = true
		result.Version = state.Version
		result.State = string(state.State)
	}
	writeJSON(w, result)
}

// probedState 只取 /api/state 中壳层关心的字段
type probedState struct {
	Version string    `json:"version"`
	State   web.State `json:"state"`
}

func probeInstance(reg *Registry, id string, parent context.Context) (*probedState, error) {
	in, ok := reg.Get(id)
	if !ok {
		return nil, errors.New("实例不存在")
	}
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(in.URL, "/")+"/api/state", nil)
	if err != nil {
		return nil, err
	}
	if in.Token != "" {
		req.Header.Set("Authorization", "Bearer "+in.Token)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("鉴权失败：令牌缺失或不正确")
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	var st probedState
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	return &st, nil
}

var probeClient = &http.Client{Timeout: 8 * time.Second} //nolint:govet // 共享客户端无敏感状态

func (s *Shell) handleAutostartGet(w http.ResponseWriter, _ *http.Request) {
	enabled, err := s.autostart.Enabled()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"enabled": enabled})
}

func (s *Shell) handleAutostartSet(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[struct {
		Enabled bool `json:"enabled"`
	}](w, r)
	if !ok {
		return
	}
	var err error
	if body.Enabled {
		err = s.autostart.Enable()
	} else {
		err = s.autostart.Disable()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"enabled": body.Enabled})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
