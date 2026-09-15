package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseSel(t *testing.T) {
	sel := parseSel(`[["openai-official","gpt-4o"],["deepseek","deepseek-chat"]]`)
	if sel == nil {
		t.Fatal("expected non-nil sel")
	}
	if sel.count() != 2 {
		t.Fatalf("count = %d, want 2", sel.count())
	}
	if !sel.has("openai-official", "gpt-4o") {
		t.Error("openai-official/gpt-4o should be selected")
	}
	if sel.has("openai-official", "gpt-4o-mini") {
		t.Error("gpt-4o-mini should NOT be selected")
	}
	if sel.has("other-vendor", "anything") {
		t.Error("other-vendor should NOT be selected")
	}
	// empty / garbage
	if parseSel("") != nil {
		t.Error("empty raw should be nil")
	}
	if parseSel("not-json") != nil {
		t.Error("garbage raw should be nil")
	}
	if parseSel("[]") != nil {
		t.Error("empty array should be nil")
	}
}

func TestParseProviderHeaders(t *testing.T) {
	cfgYAML := `
openai-compatibility:
  - name: openrouter
    base-url: https://openrouter.ai/api/v1
    headers:
      HTTP-Referer: "https://example.com"
      X-Title: "My Proxy"
      X-Session-Id: "$ABC"      # 动态值，探测时应跳过
      User-Agent: "Custom-UA"    # 应保留原始大小写并可覆盖默认 UA
    api-key-entries:
      - api-key: sk-test
    models:
      - name: m1
  - name: plain
    base-url: https://api.b.com
    api-key: k
    models:
      - name: m2
`
	pc := parseCoreConfig(cfgYAML)
	p := pc.providers["openrouter"]
	if p == nil {
		t.Fatal("openrouter provider missing")
	}
	if p.Headers["HTTP-Referer"] != "https://example.com" {
		t.Errorf("HTTP-Referer = %q", p.Headers["HTTP-Referer"])
	}
	if p.Headers["X-Title"] != "My Proxy" {
		t.Errorf("X-Title = %q", p.Headers["X-Title"])
	}
	if _, ok := p.Headers["X-Session-Id"]; !ok {
		t.Error("解析层应保留 $ 动态值原样（与宿主语义一致），过滤交给 providerHeaders")
	}
	if p.Headers["User-Agent"] != "Custom-UA" {
		t.Errorf("User-Agent = %q，键大小写应保留", p.Headers["User-Agent"])
	}
	// 解析层保留 $ 原值（与宿主语义一致），过滤发生在 providerHeaders
	if p.Headers["X-Session-Id"] != "$ABC" {
		t.Errorf("X-Session-Id 解析值 = %q，应为 $ABC（引号已剥离）", p.Headers["X-Session-Id"])
	}

	// providerHeaders 过滤逻辑
	hdrs := providerHeaders(p)
	if len(hdrs["HTTP-Referer"]) != 1 || hdrs["HTTP-Referer"][0] != "https://example.com" {
		t.Errorf("providerHeaders HTTP-Referer = %v", hdrs["HTTP-Referer"])
	}
	if _, ok := hdrs["X-Session-Id"]; ok {
		t.Error("$ 前缀动态头应在 providerHeaders 中被跳过")
	}
	if hdrs["User-Agent"][0] != "Custom-UA" {
		t.Errorf("providerHeaders User-Agent = %v", hdrs["User-Agent"])
	}
	// 无 headers 的供应商返回空 map
	empty := providerHeaders(pc.providers["plain"])
	if len(empty) != 0 {
		t.Errorf("plain provider headers = %v, want empty", empty)
	}
	nilHeaders := providerHeaders(nil)
	if len(nilHeaders) != 0 {
		t.Error("nil provider should give empty headers")
	}
}

func TestSelectionCardHTML(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
openai-compatibility:
  - name: openai-official
    base-url: https://api.openai.com/v1
    api-key-entries:
      - api-key: sk-test1
    models:
      - name: gpt-4o
        alias: gpt-4o-2024
        max-context-length: 128000
      - name: gpt-4o-mini
  - name: dead-vendor
    base-url: https://api.example.com
    api-key-entries:
      - api-key: sk-test2
    disabled: true
    models:
      - name: legacy-model
`), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := cfg.ConfigPath
	cfg.ConfigPath = cfgPath
	defer func() { cfg.ConfigPath = prev }()

	html := selectionCardHTML(nil, false)
	if !strings.Contains(html, "openai-official") {
		t.Error("provider name missing")
	}
	if !strings.Contains(html, "gpt-4o") || !strings.Contains(html, "gpt-4o-mini") {
		t.Error("model missing")
	}
	if !strings.Contains(html, "gpt-4o-2024") {
		t.Error("alias missing")
	}
	if !strings.Contains(html, "128000") {
		t.Error("existing max-context-length missing")
	}
	if !strings.Contains(html, "已停用") {
		t.Error("disabled provider badge missing")
	}
	// disabled 供应商的模型 checkbox 应带 disabled 属性
	deadIdx := strings.Index(html, `data-prov="dead-vendor"`)
	if deadIdx < 0 {
		t.Fatal("dead-vendor checkbox missing")
	}
	if !strings.Contains(html[deadIdx:deadIdx+120], "disabled") {
		t.Error("disabled provider checkbox should carry disabled attr")
	}
	// 可探测供应商的模型 checkbox 不应 disabled
	okIdx := strings.Index(html, `data-prov="openai-official"`)
	if okIdx < 0 {
		t.Fatal("openai-official checkbox missing")
	}
	if strings.Contains(html[okIdx:okIdx+120], "disabled") {
		t.Error("openai-official checkbox should not be disabled")
	}
}

func TestSelectionFiltersProbe(t *testing.T) {
	// 验证选中集合在 runProbe 匹配逻辑中的行为（通过构建一个临时 config + sel 做定点计划计数）
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
openai-compatibility:
  - name: v1
    base-url: https://api.a.com
    api-key: k
    models:
      - name: m1
      - name: m2
  - name: v2
    base-url: https://api.b.com
    api-key: k
    models:
      - name: m3
`), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := cfg.ConfigPath
	cfg.ConfigPath = cfgPath
	defer func() { cfg.ConfigPath = prev }()

	pc := parseCoreConfig(mustRead(cfgPath))
	sel := parseSel(`[["v1","m2"],["v2","m3"]]`)
	if sel == nil {
		t.Fatal("sel nil")
	}

	planned := 0
	for _, pname := range pc.order {
		if _, ok := sel[pname]; !ok {
			continue
		}
		for _, me := range pc.modelsOf(pname) {
			if sel.has(me.provider, me.name) || sel.has(me.provider, me.public) {
				planned++
			}
		}
	}
	if planned != 2 {
		t.Fatalf("planned = %d, want 2 (m2,m3)", planned)
	}
}

func mustRead(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// 布局：有报告时选择卡折叠且位于结果表之后（结果优先）；无报告时展开在顶部
func selCardClass(html string) string {
	re := regexp.MustCompile(`<div class="card[^"]*" id="cp-sel-card"`)
	return re.FindString(html)
}

func writeMiniCfg(t *testing.T) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
openai-compatibility:
  - name: v1
    base-url: http://127.0.0.1:1
    api-key-entries:
      - api-key: k
    models:
      - name: m1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestLayoutWithReport(t *testing.T) {
	cfg.ConfigPath = writeMiniCfg(t)
	rep := &probeReport{
		ProbedAt: "2026-01-01 00:00:00",
		Models:   1, Probed: 1, Total: 1, OK: 1,
		Providers: map[string]map[string]*modelResult{
			"v1": {"m1": {Provider: "v1", Name: "m1", Public: "m1", Context: 100, SrcCtx: "元数据", Status: "ok"}},
		},
	}
	probeMu.Lock()
	last = rep
	running = false
	probeMu.Unlock()

	html := string(statusHTML())
	cardIdx := strings.Index(html, `id="cp-sel-card"`)
	resIdx := strings.Index(html, "res-table")
	if cardIdx < 0 || resIdx < 0 {
		t.Fatalf("card=%d res=%d", cardIdx, resIdx)
	}
	if cardIdx < resIdx {
		t.Error("选择卡片应在结果表之后（结果优先）")
	}
	if !strings.Contains(selCardClass(html), "collapsed") {
		t.Error("有报告时选择卡应默认折叠")
	}
	if !strings.Contains(html, "btn-sel-toggle") {
		t.Error("缺少折叠开关")
	}
	if !strings.Contains(html, `class="mini cp-probe"`) {
		t.Error("重试链接应带 cp-probe class（JS 内联反馈，不跳页）")
	}
}

func TestLayoutNoReport(t *testing.T) {
	cfg.ConfigPath = writeMiniCfg(t)
	probeMu.Lock()
	last = nil
	running = false
	probeMu.Unlock()

	html := string(statusHTML())
	if cls := selCardClass(html); cls == "" {
		t.Fatal("无报告时应有选择卡")
	} else if strings.Contains(cls, "collapsed") {
		t.Errorf("无报告时选择卡应展开, got %q", cls)
	}
}
