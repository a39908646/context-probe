package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// updateVersionRefs 由 `go test -run TestReadmeVersionInSync -update` 传入，
// 用于把 README 顶部版本行同步为 pluginVersion。
var updateVersionRefs = flag.Bool("update", false, "同步 README.md 的版本行到 pluginVersion")

// TestReadmeVersionInSync 保证 README 顶部「当前版本：**x**」与 pluginVersion 一致：
// 默认不一致即失败；加 -update 则自动改写 README（构建脚本会调用）。
func TestReadmeVersionInSync(t *testing.T) {
	const path = "README.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(当前版本：\*\*)([^*]+)(\*\*)`)
	m := re.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("%s 中未找到「当前版本：**x**」标记", path)
	}
	if m[2] == pluginVersion {
		return
	}
	if *updateVersionRefs {
		out := re.ReplaceAllString(string(raw), "${1}"+pluginVersion+"${3}")
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("README.md 版本已同步为 %s", pluginVersion)
		return
	}
	t.Fatalf("README 版本 %q 与 pluginVersion %q 不一致（运行 go test -run TestReadmeVersionInSync -update 同步）", m[2], pluginVersion)
}

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
      X-Session-Id: "$ABC"      # 动态值，测试时应跳过
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
	if p.Headers["User-Agent"] != "Custom-UA" {
		t.Errorf("User-Agent = %q，键大小写应保留", p.Headers["User-Agent"])
	}
	if p.Headers["X-Session-Id"] != "$ABC" {
		t.Errorf("X-Session-Id 解析值 = %q，应为 $ABC（引号已剥离）", p.Headers["X-Session-Id"])
	}

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
	if len(providerHeaders(pc.providers["plain"])) != 0 {
		t.Error("plain provider headers should be empty")
	}
	if len(providerHeaders(nil)) != 0 {
		t.Error("nil provider should give empty headers")
	}
}

const listCfg = `
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
`

func TestListHTML(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(listCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := cfg.ConfigPath
	cfg.ConfigPath = cfgPath
	defer func() { cfg.ConfigPath = prev }()
	mu.Lock()
	last = nil
	running = false
	mu.Unlock()

	html := string(listHTML())
	if !strings.Contains(html, "openai-official") {
		t.Error("provider name missing")
	}
	if !strings.Contains(html, "gpt-4o") || !strings.Contains(html, "gpt-4o-mini") {
		t.Error("model missing")
	}
	if !strings.Contains(html, `value="128000"`) {
		t.Error("existing max-context-length should be pre-filled in the input")
	}
	if !strings.Contains(html, `class="inp in-ctx"`) || !strings.Contains(html, `class="inp in-out"`) {
		t.Error("缺少可编辑的上下文 / 输出上限输入框")
	}
	if !strings.Contains(html, `id="btn-test"`) {
		t.Error("缺少「按 config 值测试」按钮")
	}
	if !strings.Contains(html, `id="btn-save"`) {
		t.Error("缺少「保存并回写 config」按钮")
	}
	if !strings.Contains(html, `id="cp-only-empty"`) {
		t.Error("缺少「仅看上下文为空」筛选")
	}
	if !strings.Contains(html, `id="btn-batch-ctx"`) || !strings.Contains(html, `id="btn-batch-out"`) {
		t.Error("缺少批量编辑按钮")
	}
	// 未启用/不可探测的供应商不应展示
	if strings.Contains(html, "dead-vendor") || strings.Contains(html, "legacy-model") {
		t.Error("未启用的供应商不应出现在模型列表页")
	}
}

func TestOverrideOutputs(t *testing.T) {
	text := `
payload:
  override:
    - "models":
        - "name": "gpt-4o"
          "protocol": "openai"
      "params":
        "max_tokens": 4096
    - "models":
        - "name": "gpt-4o"
        - "name": "claude-3"
      "params":
        "max_tokens": 8192
`
	outs := overrideOutputs(splitLines(text))
	if outs["gpt-4o"] != 8192 {
		t.Errorf("gpt-4o output = %d, want 8192 (last rule wins)", outs["gpt-4o"])
	}
	if outs["claude-3"] != 8192 {
		t.Errorf("claude-3 output = %d, want 8192", outs["claude-3"])
	}
	if len(overrideOutputs(splitLines("other: 1\n"))) != 0 {
		t.Error("no payload section should yield empty map")
	}
}

func TestApplyEditsWritesContextAndOutput(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(listCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := cfg.ConfigPath
	cfg.ConfigPath = cfgPath
	defer func() { cfg.ConfigPath = prev }()

	changes, err := applyEdits([]modelEdit{
		{Provider: "openai-official", Name: "gpt-4o-mini", Public: "gpt-4o-mini", Context: 64000},
		{Provider: "openai-official", Name: "gpt-4o", Public: "gpt-4o-2024", Output: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %v, want 2 entries", changes)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pc := parseCoreConfig(string(raw))
	vals := map[string]int{}
	for _, me := range pc.models {
		vals[me.name] = me.maxclVal
	}
	if vals["gpt-4o-mini"] != 64000 {
		t.Errorf("gpt-4o-mini max-context-length = %d, want 64000", vals["gpt-4o-mini"])
	}
	if vals["gpt-4o"] != 128000 {
		t.Errorf("gpt-4o max-context-length = %d, want unchanged 128000", vals["gpt-4o"])
	}
	outs := overrideOutputs(splitLines(string(raw)))
	if outs["gpt-4o-2024"] != 4096 {
		t.Errorf("gpt-4o-2024 output = %d, want 4096", outs["gpt-4o-2024"])
	}
}

func TestApplyEditsNoChange(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(listCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := cfg.ConfigPath
	cfg.ConfigPath = cfgPath
	defer func() { cfg.ConfigPath = prev }()

	before, _ := os.ReadFile(cfgPath)
	changes, err := applyEdits([]modelEdit{
		{Provider: "openai-official", Name: "gpt-4o", Public: "gpt-4o-2024", Context: 128000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %v, want none (same value)", changes)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Error("config file should be untouched when value is unchanged")
	}
}

func TestStatusJSONShape(t *testing.T) {
	mu.Lock()
	last = &testRun{
		StartedAt: "2026-01-01 00:00:00",
		Total:     1, Done: 1,
		Results: map[string]map[string]*modelResult{
			"v1": {"m1": {Provider: "v1", Name: "m1", Public: "m1", Status: "ok", Detail: "HTTP 200"}},
		},
	}
	running = false
	mu.Unlock()

	d := statusJSON()
	if d["running"] != false {
		t.Error("running should be false")
	}
	if d["total"] != 1 || d["done"] != 1 {
		t.Errorf("total/done = %v/%v, want 1/1", d["total"], d["done"])
	}
	rs, ok := d["results"].(map[string]map[string]*modelResult)
	if !ok || rs["v1"]["m1"].Status != "ok" {
		t.Error("results missing")
	}
}
