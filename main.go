// context-probe —— CLIProxyAPI 标准动态库插件（Management API 能力）。
//
// 功能：探测各 openai-compatibility 渠道的真实上下文/输出上限，并把精确的
// max-context-length 写回 config.yaml（宿主 file watcher 自动热加载）。
//
// 值解析顺序：元数据 > 报错提取 > 已接受(声称值) > 映射表回落(仅未校验渠道) > 保持现值。
// 映射表(model-context-map.json)只作为探测未校验(未知)渠道的回落值，无独立写入模式。
// 仅写回 max-context-length；payload.override 仍由脚本/手动管理（全局按名匹配）。
//
// 安装位置：plugins/ 或 plugins/<goos>/<goarch>/，文件名即插件 ID（context-probe.dll）。
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const abiVersion uint32 = 1
const rpcSchemaVersion uint32 = 6 // 与宿主 pluginabi.SchemaVersion 一致，raw JSON 响应（不 HTML 转义）

// ------------------------- ABI -------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {}

var hostInitOnce sync.Once

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var reqRaw []byte
	if request != nil && requestLen > 0 {
		reqRaw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, errHandle := handleMethod(C.GoString(method), reqRaw)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, result)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	probeMu.Lock()
	probeStop = true
	probeMu.Unlock()
}

func okEnvelope(result any) []byte {
	raw, err := json.Marshal(envelope{OK: true, Result: mustJSON(result)})
	if err != nil {
		return errorEnvelope("marshal", err.Error())
	}
	return raw
}

func okEnvelopeRaw(rawResult []byte) []byte {
	raw, _ := json.Marshal(envelope{OK: true, Result: rawResult})
	return raw
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func callHost(method string, payload []byte) (json.RawMessage, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(unsafe.Pointer(&payload[0]))
	}
	var response C.cliproxy_buffer
	if C.call_host_api(cMethod, req, C.size_t(len(payload)), &response) != 0 || response.ptr == nil {
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	raw := C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
	C.free_host_buffer(unsafe.Pointer(response.ptr), response.len)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host %s envelope: %w", method, err)
	}
	if !env.OK {
		code, msg := "host_error", ""
		if env.Error != nil {
			code, msg = env.Error.Code, env.Error.Message
		}
		return nil, fmt.Errorf("host %s error: %s: %s", method, code, msg)
	}
	return env.Result, nil
}

// ------------------------- 状态 -------------------------

type pluginConfig struct {
	ConfigPath   string
	MappingPath  string
	Providers    string
	TimeoutSecs  int
	DelaySecs    float64
	AutoApply    bool
	MetadataOnly bool
}

type modelResult struct {
	Provider string   `json:"provider"`
	Name     string   `json:"name"`
	Public   string   `json:"public"`
	Context  int      `json:"context,omitempty"`
	SrcCtx   string   `json:"src-context,omitempty"`
	Output   int      `json:"output,omitempty"`
	SrcOut   string   `json:"src-output,omitempty"`
	Status   string   `json:"status"`
	Detail   []string `json:"detail,omitempty"`
}

type probeReport struct {
	ProbedAt  string                             `json:"probed-at"`
	Models    int                                `json:"models"`  // 计划探测总数
	Probed    int                                `json:"probed"`  // 已完成数
	Current   string                             `json:"current"` // 正在探测的模型
	Total     int                                `json:"total"`
	OK        int                                `json:"ok"`
	Dead      int                                `json:"dead"`
	Unknown   int                                `json:"unknown"`
	Providers map[string]map[string]*modelResult `json:"providers"`
	Applied   []string                           `json:"applied,omitempty"`
}

var (
	cfg       = pluginConfig{TimeoutSecs: 60, DelaySecs: 0.3, AutoApply: true}
	probeMu   sync.Mutex
	running   bool
	last      *probeReport
	probeStop bool
)

// ------------------------- 方法分发 -------------------------

func handleMethod(method string, reqRaw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return handleLifecycle(method, reqRaw)
	case "management.register":
		return okEnvelope(map[string]any{
			"resources": []map[string]any{{
				"Path":        "/probe",
				"Menu":        "Context Probe",
				"Description": "渠道上下文/输出上限探测与写回（元数据 > 报错提取 > 已接受 > 映射回落）",
			}},
		}), nil
	case "management.handle":
		return handleMgmt(reqRaw)
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func handleLifecycle(method string, reqRaw []byte) ([]byte, error) {
	if len(reqRaw) > 0 {
		var lr struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(reqRaw, &lr); err == nil && len(lr.ConfigYAML) > 0 {
			applyPluginConfig(string(lr.ConfigYAML))
		}
	}
	if method != "plugin.register" {
		return registrationResult(), nil
	}
	return registrationResult(), nil
}

func registrationResult() []byte {
	return okEnvelope(map[string]any{
		"schema_version": rpcSchemaVersion,
		"metadata": map[string]any{
			"Name":             "context-probe",
			"Version":          "0.3.0",
			"Author":           "cloudwayne",
			"GitHubRepository": "https://github.com/a39908646/context-probe",
			"ConfigFields": []map[string]any{
				{"Name": "config_path", "Type": "string", "Description": "config.yaml 路径（默认自动探测 cpa-core/config.yaml）"},
				{"Name": "mapping_path", "Type": "string", "Description": "映射表路径（未校验渠道回落值）"},
				{"Name": "providers", "Type": "string", "Description": "供应商过滤（子串，逗号分隔，空=全部）"},
				{"Name": "probe_timeout_seconds", "Type": "int", "Description": "单请求超时秒数（默认 60）"},
				{"Name": "probe_delay_seconds", "Type": "float", "Description": "请求间隔秒数（默认 0.3）"},
				{"Name": "auto_apply", "Type": "bool", "Description": "探测后自动写回 config.yaml（默认 true）"},
				{"Name": "metadata_only", "Type": "bool", "Description": "仅元数据模式，不发测试请求（默认 false）"},
			},
		},
		"capabilities": map[string]any{"management_api": true},
	})
}

var keyValueRe = regexp.MustCompile(`^"?([^":]+)"?\s*:\s*(.*)$`)

func applyPluginConfig(text string) {
	probeMu.Lock()
	defer probeMu.Unlock()
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(raw)-len(strings.TrimLeft(raw, " \t")) > 0 {
			continue // 只取本段顶层键
		}
		kv := keyValueRe.FindStringSubmatch(line)
		if kv == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[1]))
		val := unquote(kv[2])
		switch key {
		case "config_path":
			cfg.ConfigPath = val
		case "mapping_path":
			cfg.MappingPath = val
		case "providers":
			cfg.Providers = val
		case "probe_timeout_seconds":
			if n, err := strconv.Atoi(val); err == nil {
				cfg.TimeoutSecs = n
			}
		case "probe_delay_seconds":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				cfg.DelaySecs = f
			}
		case "auto_apply":
			cfg.AutoApply = val == "" || strings.EqualFold(val, "true")
		case "metadata_only":
			cfg.MetadataOnly = strings.EqualFold(val, "true")
		}
	}
}

// ------------------------- management.handle -------------------------

func handleMgmt(reqRaw []byte) ([]byte, error) {
	var req struct {
		Method string              `json:"Method"`
		Path   string              `json:"Path"`
		Query  map[string][]string `json:"Query"`
	}
	_ = json.Unmarshal(reqRaw, &req)
	op := ""
	if req.Query != nil {
		if v, ok := req.Query["op"]; ok && len(v) > 0 {
			op = strings.ToLower(v[0])
		}
	}
	switch op {
	case "", "status":
		return mgmtResponse(200, "text/html; charset=utf-8", statusHTML()), nil
	case "probe":
		provider, model := "", ""
		if req.Query != nil {
			if v, ok := req.Query["provider"]; ok && len(v) > 0 {
				provider = v[0]
			}
			if v, ok := req.Query["model"]; ok && len(v) > 0 {
				model = v[0]
			}
		}
		scope := "全量"
		if provider != "" {
			scope = "供应商「" + provider + "」"
			if model != "" {
				scope += " 模型「" + model + "」"
			}
		}
		started := startProbe(provider, model)
		if req.Query != nil {
			if v, ok := req.Query["format"]; ok && len(v) > 0 && v[0] == "json" {
				if started {
					return mgmtResponse(200, "application/json", mustJSONIndent(map[string]any{"ok": true, "started": true, "scope": scope})), nil
				}
				return mgmtResponse(200, "application/json", mustJSONIndent(map[string]any{"ok": true, "started": false, "scope": "已在运行中"})), nil
			}
		}
		if started {
			return mgmtResponse(200, "text/html; charset=utf-8", renderPage("已开始探测",
				`<div class="wrap"><h1>已开始探测（`+html.EscapeString(scope)+`）</h1><p>探测在后台运行。</p><p><a class="btn btn-primary" href="?op=status">查看状态</a></p></div>`, 0)), nil
		}
		return mgmtResponse(200, "text/html; charset=utf-8", renderPage("探测已在运行",
			`<div class="wrap"><h1>探测已在运行中</h1><p><a class="btn btn-primary" href="?op=status">查看状态</a></p></div>`, 0)), nil
	case "apply":
		isJSON := false
		if req.Query != nil {
			if v, ok := req.Query["format"]; ok && len(v) > 0 && v[0] == "json" {
				isJSON = true
			}
		}
		probeMu.Lock()
		rep := last
		probeMu.Unlock()
		if isJSON {
			if rep == nil {
				return mgmtResponse(200, "application/json", mustJSONIndent(map[string]any{"ok": false, "error": "尚无探测报告，请先运行探测"})), nil
			}
			changes, aerr := applyReport(rep)
			if aerr != nil {
				return mgmtResponse(200, "application/json", mustJSONIndent(map[string]any{"ok": false, "error": aerr.Error()})), nil
			}
			return mgmtResponse(200, "application/json", mustJSONIndent(map[string]any{"ok": true, "changes": changes})), nil
		}
		if rep == nil {
			return mgmtResponse(200, "text/html; charset=utf-8", renderPage("无报告",
				"<h1>尚无探测报告</h1><p>请先 <a href=\"?op=probe\">运行探测</a>。</p>", 0)), nil
		}
		changes, err := applyReport(rep)
		var body string
		if err != nil {
			body = "<h1>写回失败</h1><p>" + html.EscapeString(err.Error()) + "</p><p><a href=\"?op=status\">返回状态</a></p>"
		} else {
			parts := []string{"<h1>写回完成</h1><ul>"}
			for _, c := range changes {
				parts = append(parts, "<li>"+html.EscapeString(c)+"</li>")
			}
			parts = append(parts, "</ul><p><a href=\"?op=status\">返回状态</a></p>")
			body = strings.Join(parts, "")
		}
		return mgmtResponse(200, "text/html; charset=utf-8", renderPage("写回结果", body, 0)), nil
	case "report":
		if configPath, err := resolveConfigPath(); err == nil {
			p := filepath.Join(filepath.Dir(configPath), "model-context-probe.json")
			if raw, err := os.ReadFile(p); err == nil {
				return mgmtResponse(200, "application/json", raw), nil
			}
		}
		probeMu.Lock()
		rep := last
		probeMu.Unlock()
		if rep == nil {
			return mgmtResponse(404, "text/plain; charset=utf-8", []byte("尚无报告")), nil
		}
		return mgmtResponse(200, "application/json", mustJSONIndent(rep)), nil
	default:
		return mgmtResponse(404, "text/plain; charset=utf-8", []byte("unknown op: "+op)), nil
	}
}

func mgmtResponse(status int, contentType string, body []byte) []byte {
	raw, _ := json.Marshal(map[string]any{
		"StatusCode": status,
		"Headers":    map[string][]string{"content-type": {contentType}},
		"Body":       body, // []byte → base64
	})
	return okEnvelopeRaw(raw)
}

// ------------------------- HTML 页面 -------------------------

func renderPage(title, bodyHTML string, refresh int) []byte {
	// 常驻脚本：状态标签点击筛选（挂 window 上，软刷新后状态保留）
	script := `<script>
(function(){
window.cpFilter=null;
window.cpApply=function(){
  var f=window.cpFilter||'';
  document.querySelectorAll('.chip[data-filter]').forEach(function(c){
    c.classList.toggle('chip-active',f!==''&&c.dataset.filter===f);
  });
  document.querySelectorAll('table tbody tr').forEach(function(tr){
    var td=tr.querySelector('td.st-ok,td.st-dead,td.st-unknown');
    var s=td?(td.classList.contains('st-ok')?'ok':td.classList.contains('st-dead')?'dead':'unknown'):'';
    tr.style.display=(f===''||f===s)?'':'none';
  });
};
window.cpToast=function(title,lines){
  var t=document.getElementById('cp-toast');
  if(!t)return;
  document.getElementById('cp-toast-title').textContent=title||'';
  var u=document.getElementById('cp-toast-list');
  u.innerHTML='';
  (lines||[]).forEach(function(l){var li=document.createElement('li');li.textContent=l;u.appendChild(li);});
  document.getElementById('cp-toast-empty').style.display=(lines&&lines.length)?'none':'block';
  t.classList.add('show');
};
function swapBody(doc){
  var toast=document.getElementById('cp-toast');
  var fresh=doc.getElementById('cp-toast');
  if(fresh&&fresh.parentNode)fresh.parentNode.removeChild(fresh);
  var nodes=[toast];
  doc.body.childNodes.forEach(function(n){nodes.push(n)});
  document.body.replaceChildren.apply(document.body,nodes);
  if(window.cpApply)window.cpApply();
}
function refreshOnce(){
  fetch('?op=status',{cache:'no-store'}).then(function(r){return r.text()}).then(function(html){
    swapBody(new DOMParser().parseFromString(html,'text/html'));
  }).catch(function(){});
}
var polling=false;
function sleep(ms){return new Promise(function(res){setTimeout(res,ms)})}
async function pollLoop(){
  for(;;){
    await sleep(3000);
    try{
      var res=await fetch('?op=status',{cache:'no-store'});
      var html=await res.text();
      var doc=new DOMParser().parseFromString(html,'text/html');
      var keep=!!doc.getElementById('cp-autorefresh');
      swapBody(doc);
      if(!keep){polling=false;return;}
    }catch(e){}
  }
}
window.cpEnsurePoll=function(){if(!polling){polling=true;pollLoop();}};
if(document.getElementById('cp-autorefresh'))window.cpEnsurePoll();
document.addEventListener('click',function(e){
  var ab=e.target.closest('#btn-apply');
  if(ab){
    e.preventDefault();
    ab.textContent='⬇ 写回中…';
    fetch('?op=apply&format=json',{cache:'no-store'}).then(function(r){return r.json()}).then(function(d){
      ab.textContent='⬇ 应用写回';
      if(d.ok){window.cpToast('写回完成',d.changes||[]);refreshOnce();}
      else{window.cpToast('写回失败',[d.error||'未知错误']);}
    }).catch(function(err){ab.textContent='⬇ 应用写回';window.cpToast('写回失败',[String(err)]);});
    return;
  }
  var pb=e.target.closest('#btn-probe');
  if(pb){
    e.preventDefault();
    fetch('?op=probe&format=json',{cache:'no-store'}).then(function(r){return r.json()}).then(function(d){
      if(!d.ok){window.cpToast('操作失败',[d.error||'未知错误']);return;}
      window.cpToast(d.started?'已开始探测':'探测已在运行中',d.started&&d.scope?['范围：'+d.scope,'页面将自动刷新进度']:[]);
      setTimeout(function(){refreshOnce();window.cpEnsurePoll();},800);
    }).catch(function(err){window.cpToast('操作失败',[String(err)]);});
    return;
  }
  var x=e.target.closest('#cp-toast-close');
  if(x){document.getElementById('cp-toast').classList.remove('show');return;}
  var c=e.target.closest('.chip[data-filter]');
  if(!c)return;
  e.preventDefault();
  window.cpFilter=(window.cpFilter===c.dataset.filter)?'':c.dataset.filter;
  window.cpApply();
});
})();
</script>`
	if refresh > 0 {
		// 软刷新：fetch 局部替换 body，避免整页重载闪烁
		script += fmt.Sprintf(`<script>
async function poll(){
  try{
    const res=await fetch('?op=status',{cache:'no-store'});
    const html=await res.text();
    const doc=new DOMParser().parseFromString(html,'text/html');
    document.body.replaceChildren(...doc.body.childNodes);
    if(window.cpApply)window.cpApply();
  }catch(e){}
  setTimeout(poll,%d000);
}
setTimeout(poll,%d000);
</script>`, refresh, refresh)
	}
	// 样式与脚本放在 <body> 内：宿主 SPA 可能只抽取 body 内容嵌入渲染
	s := `<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><title>` + html.EscapeString(title) + `</title></head><body>` +
		script +
		`<style>
/* 亮色默认 + 跟随系统深色模式自动切换 */
:root{--bg:#ffffff;--panel:#f6f8fa;--border:#d0d7de;--text:#1f2328;--muted:#656d76;
--accent:#0969da;--ok:#1a7f37;--dead:#cf222e;--warn:#9a6700;--row-alt:#f6f8fa;--th-bg:#f6f8fa;--chip-bg:#ffffff}
@media (prefers-color-scheme: dark){:root{--bg:#0d1117;--panel:#161b22;--border:#30363d;--text:#e6edf3;--muted:#8b949e;
--accent:#2f81f7;--ok:#3fb950;--dead:#f85149;--warn:#d29922;--row-alt:#11151c;--th-bg:#1c2128;--chip-bg:#0d1117}}
*{box-sizing:border-box}
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;margin:0;background:var(--bg);color:var(--text);font-size:14px;line-height:1.6}
.wrap{max-width:1100px;margin:0 auto;padding:24px 20px 48px}
h1{font-size:20px;margin:0 0 16px;font-weight:600}
h2{font-size:15px;margin:28px 0 8px;font-weight:600;border-bottom:1px solid var(--border);padding-bottom:6px}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
.btn{display:inline-block;padding:7px 16px;border-radius:6px;font-size:13px;font-weight:500;
border:1px solid var(--border);background:var(--panel);color:var(--text);cursor:pointer;transition:background .15s}
.btn:hover{background:var(--row-alt);text-decoration:none}
.btn-primary{background:var(--accent);border-color:var(--accent);color:#fff}
.btn-primary:hover{background:#1f6feb}
.toolbar{display:flex;gap:10px;align-items:center;margin-bottom:16px;flex-wrap:wrap}
.card{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:16px;margin-bottom:16px}
.muted{color:var(--muted);font-size:12px}
.state-run{color:var(--warn);font-weight:600}
.state-idle{color:var(--ok);font-weight:600}
.progress{height:10px;background:var(--row-alt);border-radius:5px;overflow:hidden;margin:8px 0 6px}
.progress .bar{height:100%;background:linear-gradient(90deg,var(--accent),#58a6ff);transition:width .4s;border-radius:5px}
.stat-chips{display:flex;gap:8px;flex-wrap:wrap}
.chip{display:inline-flex;align-items:center;gap:6px;padding:3px 12px;border-radius:12px;font-size:12px;border:1px solid var(--border);background:var(--chip-bg)}
.chip .n{font-weight:700}
.chip-ok .n{color:var(--ok)}.chip-dead .n{color:var(--dead)}.chip-unknown .n{color:var(--warn)}
.chip[data-filter]{cursor:pointer;user-select:none;transition:border-color .15s}
.chip[data-filter]:hover{border-color:var(--accent)}
.chip-active{border-color:var(--accent)!important;background:var(--accent);color:#fff}
.chip-active .n,.chip-active .dot-ok,.chip-active .dot-dead,.chip-active .dot-warn{color:#fff!important}
.chip-active .dot-ok,.chip-active .dot-dead,.chip-active .dot-warn{background:#fff!important}
.dot-ok,.dot-dead,.dot-warn{display:inline-block;width:8px;height:8px;border-radius:50%}
.dot-ok{background:var(--ok)}.dot-dead{background:var(--dead)}.dot-warn{background:var(--warn)}
table{border-collapse:collapse;width:100%;margin-top:6px;font-size:13px;table-layout:fixed}
th,td{border:1px solid var(--border);padding:6px 10px;text-align:left;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
th{background:var(--th-bg);font-weight:600}
td.wrap-cell{white-space:normal;word-break:break-all}
tr:nth-child(even) td{background:var(--row-alt)}
th:nth-child(1),td:nth-child(1){width:24%}
th:nth-child(2),td:nth-child(2){width:13%}
th:nth-child(3),td:nth-child(3){width:10%}
th:nth-child(4),td:nth-child(4){width:13%}
th:nth-child(5),td:nth-child(5){width:10%}
th:nth-child(6),td:nth-child(6){width:9%}
.st-ok{color:var(--ok)}.st-dead{color:var(--dead)}.st-unknown{color:var(--warn)}
code{background:var(--th-bg);padding:1px 5px;border-radius:4px;font-size:12px}
ul.applied{margin:4px 0;padding-left:20px;font-size:12px;color:var(--muted)}
ul.applied li{margin:2px 0}
.current{color:var(--muted);font-size:12px}
.current b{color:var(--accent)}
a.mini{font-size:12px;font-weight:400;margin-left:6px}
.toast{position:fixed;top:64px;right:16px;max-width:440px;background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:12px 16px;box-shadow:0 6px 24px rgba(0,0,0,.25);z-index:9999;display:none}
.toast.show{display:block}
.toast h3{margin:0 0 8px;font-size:14px;padding-right:24px}
.toast ul{max-height:320px;overflow:auto;padding-left:18px;margin:0;font-size:12px;color:var(--muted)}
.toast .empty{margin:0;font-size:12px;color:var(--muted)}
.toast-x{position:absolute;top:8px;right:10px;border:none;background:none;color:var(--muted);font-size:14px;cursor:pointer;line-height:1}
.toast-x:hover{color:var(--text)}
</style>
<div id="cp-toast" class="toast"><button id="cp-toast-close" class="toast-x" title="关闭">✕</button><h3 id="cp-toast-title"></h3><ul id="cp-toast-list"></ul><p id="cp-toast-empty" class="empty">没有需要写回的变更</p></div>` + bodyHTML + "</body></html>"
	return []byte(s)
}

func statusHTML() []byte {
	probeMu.Lock()
	rep := last
	run := running
	probeMu.Unlock()

	var b strings.Builder
	b.WriteString(`<div class="wrap">`)
	b.WriteString("<h1>Context Probe · 渠道探测</h1>")
	b.WriteString(`<div class="toolbar">` +
		`<a class="btn btn-primary" id="btn-probe" href="?op=probe">▶ 开始探测</a>` +
		`<a class="btn" id="btn-apply" href="?op=apply">⬇ 应用写回</a>` +
		`<a class="btn" href="?op=report" target="_blank">JSON 报告</a></div>`)

	state := `<span class="state-idle">● 空闲</span>`
	if run {
		state = `<span class="state-run">● 运行中…</span>`
	}
	probed := "—"
	if rep != nil {
		probed = html.EscapeString(rep.ProbedAt)
	}
	b.WriteString(`<div class="card"><p style="margin:0">状态：` + state +
		`<span class="muted">　上次探测：` + probed + `</span></p>`)

	// 进度条
	if rep != nil && rep.Models > 0 {
		pct := 100
		if run {
			pct = rep.Probed * 100 / rep.Models
			if pct > 100 {
				pct = 100
			}
		}
		remain := rep.Models - rep.Probed
		if remain < 0 {
			remain = 0
		}
		b.WriteString(fmt.Sprintf(`<div class="progress"><div class="bar" style="width:%d%%"></div></div>`, pct))
		b.WriteString(fmt.Sprintf(`<p class="current" style="margin:0">进度：<b>%d / %d</b>（剩余 %d）`, rep.Probed, rep.Models, remain))
		if run && rep.Current != "" {
			b.WriteString(" · 正在探测 <b>" + html.EscapeString(rep.Current) + "</b>")
		}
		b.WriteString("</p>")
	}
	b.WriteString("</div>")

	b.WriteString(`<p class="muted">config_path=` + html.EscapeString(cfg.ConfigPath) +
		" · mapping_path=" + html.EscapeString(cfg.MappingPath) +
		" · providers=[" + html.EscapeString(cfg.Providers) + "]" +
		" · auto_apply=" + strconv.FormatBool(cfg.AutoApply) +
		" · timeout=" + strconv.Itoa(cfg.TimeoutSecs) + "s" +
		" · delay=" + strconv.FormatFloat(cfg.DelaySecs, 'f', -1, 64) + "s</p>")

	if rep == nil {
		b.WriteString("<div class=\"card\"><p>尚未运行探测，点击上方「开始探测」。</p></div>")
		b.WriteString(`<p class="muted">说明：探测并写回 max-context-length（模型项）与 payload.override 的 max_tokens（输出上限，按请求名追加规则，last-write-wins）。</p></div>`)
		return renderPage("context-probe", b.String(), 0)
	}

	// 实时统计（不依赖探测结束后的汇总字段）
	statTotal, statOK, statDead, statUnknown := 0, 0, 0, 0
	for _, pm := range rep.Providers {
		for _, r := range pm {
			statTotal++
			switch r.Status {
			case "ok":
				statOK++
			case "dead":
				statDead++
			default:
				statUnknown++
			}
		}
	}
	b.WriteString(`<div class="card"><div class="stat-chips">` +
		`<span class="chip" data-filter=""><span class="n">` + strconv.Itoa(statTotal) + `</span> 合计</span>` +
		fmt.Sprintf(`<span class="chip chip-ok" data-filter="ok" title="点击筛选可用模型"><span class="dot-ok"></span>可用 <span class="n">%d</span></span>`, statOK) +
		fmt.Sprintf(`<span class="chip chip-dead" data-filter="dead" title="点击筛选死渠道"><span class="dot-dead"></span>死渠道 <span class="n">%d</span></span>`, statDead) +
		fmt.Sprintf(`<span class="chip chip-unknown" data-filter="unknown" title="点击筛选未知模型"><span class="dot-warn"></span>未知 <span class="n">%d</span></span>`, statUnknown) +
		"</div></div>")

	if len(rep.Applied) > 0 {
		b.WriteString("<h2>最近写回</h2><ul>")
		for _, c := range rep.Applied {
			b.WriteString("<li>" + html.EscapeString(c) + "</li>")
		}
		b.WriteString("</ul>")
	}

	providers := make([]string, 0, len(rep.Providers))
	for p := range rep.Providers {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	for _, pname := range providers {
		models := rep.Providers[pname]
		names := make([]string, 0, len(models))
		for n := range models {
			names = append(names, n)
		}
		sort.Strings(names)
		q := url.QueryEscape(pname)
		b.WriteString("<h2>" + html.EscapeString(pname) +
			` <a class="mini" href="?op=probe&provider=` + q + `">↻ 重试该供应商</a></h2>`)
		b.WriteString("<table><thead><tr><th>模型</th><th>上下文</th><th>来源</th><th>输出上限</th><th>来源</th><th>状态</th><th>详情</th></tr></thead><tbody>")
		for _, n := range names {
			r := models[n]
			cls, label := "st-ok", r.Status
			switch r.Status {
			case "dead":
				cls, label = "st-dead", "死渠道"
			case "unknown":
				cls, label = "st-unknown", "未知"
			}
			ctxV, outV := "—", "—"
			if r.Context > 0 {
				ctxV = strconv.Itoa(r.Context)
			}
			if r.Output > 0 {
				outV = strconv.Itoa(r.Output)
			}
			detail := strings.Join(r.Detail, "; ")
			mq := url.QueryEscape(firstNonEmpty(r.Name, r.Public))
			b.WriteString("<tr><td>" + html.EscapeString(r.Public) + "</td><td>" + ctxV + "</td><td>" +
				html.EscapeString(r.SrcCtx) + "</td><td>" + outV + "</td><td>" +
				html.EscapeString(r.SrcOut) + "</td><td class=\"" + cls + "\">" + label +
				` <a class="mini" href="?op=probe&provider=` + q + `&model=` + mq + `">↻</a></td><td class=` + "\"wrap-cell muted\">" +
				html.EscapeString(detail) + "</td></tr>")
		}
		b.WriteString("</tbody></table>")
	}

	b.WriteString(`<p class="muted">说明：探测并写回 max-context-length（模型项）与 payload.override 的 max_tokens（输出上限，按请求名追加规则，last-write-wins）。已过期的死渠道可按本报告移除或等站方修复。</p>`)

	refresh := 0
	if run {
		refresh = 3
		b.WriteString(`<span id="cp-autorefresh" hidden></span>`)
	}
	b.WriteString("</div>")
	return renderPage("context-probe", b.String(), refresh)
}

// ------------------------- 探测引擎 -------------------------

func startProbe(onlyProvider, onlyModel string) bool {
	probeMu.Lock()
	defer probeMu.Unlock()
	if running {
		return false
	}
	running = true
	probeStop = false
	go runProbe(onlyProvider, onlyModel)
	return true
}

func nowStr() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func runProbe(onlyProvider, onlyModel string) {
	targeted := onlyProvider != ""
	var rep *probeReport
	if targeted {
		// 定点重试：合并进现有报告，不清空其它供应商结果
		probeMu.Lock()
		rep = last
		probeMu.Unlock()
		if rep == nil {
			rep = &probeReport{ProbedAt: nowStr(), Providers: map[string]map[string]*modelResult{}}
		}
		rep.ProbedAt = nowStr()
	} else {
		rep = &probeReport{ProbedAt: nowStr(), Providers: map[string]map[string]*modelResult{}}
	}
	probeMu.Lock()
	last = rep
	probeMu.Unlock()

	configPath, err := resolveConfigPath()
	if err == nil {
		raw, rerr := os.ReadFile(configPath)
		if rerr == nil {
			pc := parseCoreConfig(string(raw))
			mapping := loadMapping(mappingPathFor(configPath))
			timeout := time.Duration(cfg.TimeoutSecs) * time.Second
			if timeout <= 0 {
				timeout = 60 * time.Second
			}
			delay := time.Duration(cfg.DelaySecs * float64(time.Second))
			filter := strings.ToLower(cfg.Providers)

			matchProvider := func(pname string, p *providerInfo) bool {
				if p == nil || p.Disabled || p.BaseURL == "" || len(p.APIKeys) == 0 {
					return false
				}
				if targeted && pname != onlyProvider {
					return false
				}
				if filter != "" && !strings.Contains(strings.ToLower(pname), filter) &&
					!strings.Contains(strings.ToLower(p.BaseURL), filter) {
					return false
				}
				return true
			}
			matchModel := func(me *modelEntry) bool {
				return onlyModel == "" || me.name == onlyModel || me.public == onlyModel
			}

			// 计划探测总数（同样应用过滤条件）
			planned := 0
			for _, pname := range pc.order {
				if !matchProvider(pname, pc.providers[pname]) {
					continue
				}
				for _, me := range pc.modelsOf(pname) {
					if matchModel(me) {
						planned++
					}
				}
			}
			probeMu.Lock()
			rep.Models = planned
			rep.Probed = 0
			probeMu.Unlock()

			for _, pname := range pc.order {
				if stopped() {
					break
				}
				p := pc.providers[pname]
				if !matchProvider(pname, p) {
					continue
				}
				models := pc.modelsOf(pname)
				if len(models) == 0 {
					continue
				}
				pm := rep.Providers[pname]
				if pm == nil {
					pm = map[string]*modelResult{}
					rep.Providers[pname] = pm
				}
				key := p.APIKeys[0]
				baseURL := strings.TrimRight(p.BaseURL, "/")
				meta := fetchMetadata(baseURL, key, timeout)
				for _, me := range models {
					if stopped() {
						break
					}
					if !matchModel(me) {
						continue
					}
					r := probeModel(baseURL, key, me, meta, mapping, timeout)
					pm[r.Public] = r
					probeMu.Lock()
					rep.Probed++
					rep.Current = pname + "/" + me.name
					rep.Total++
					switch r.Status {
					case "ok":
						rep.OK++
					case "dead":
						rep.Dead++
					default:
						rep.Unknown++
					}
					probeMu.Unlock()
					if delay > 0 {
						time.Sleep(delay)
					}
				}
			}

			// 重新计数（定点重试时避免重复累加）
			total, ok, dead, unknown := 0, 0, 0, 0
			for _, pm := range rep.Providers {
				for _, r := range pm {
					total++
					switch r.Status {
					case "ok":
						ok++
					case "dead":
						dead++
					default:
						unknown++
					}
				}
			}
			probeMu.Lock()
			rep.Current = ""
			rep.Total, rep.OK, rep.Dead, rep.Unknown = total, ok, dead, unknown
			probeMu.Unlock()

			if cfg.AutoApply {
				if changes, aerr := applyReport(rep); aerr == nil && len(changes) > 0 {
					rep.Applied = changes
				}
			}
			_ = os.WriteFile(filepath.Join(filepath.Dir(configPath), "model-context-probe.json"),
				mustJSONIndent(rep), 0o644)
		} else {
			err = rerr
		}
	}

	probeMu.Lock()
	last = rep
	running = false
	probeMu.Unlock()
	_ = err
}

func stopped() bool {
	probeMu.Lock()
	defer probeMu.Unlock()
	return probeStop
}

func resolveConfigPath() (string, error) {
	cands := []string{}
	if cfg.ConfigPath != "" {
		cands = append(cands, cfg.ConfigPath)
	}
	cands = append(cands, "config.yaml", "cpa-core/config.yaml", "../config.yaml", "../cpa-core/config.yaml")
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("找不到 config.yaml（可在 plugins.configs.context-probe.config_path 指定）")
}

func mappingPathFor(configPath string) string {
	if cfg.MappingPath != "" {
		if filepath.IsAbs(cfg.MappingPath) {
			return cfg.MappingPath
		}
		return filepath.Join(filepath.Dir(configPath), cfg.MappingPath)
	}
	p := filepath.Join(filepath.Dir(configPath), "model-context-map.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if abs, err := filepath.Abs("model-context-map.json"); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return p
}

// ------------------------- host.http.do -------------------------

func httpDo(method, target string, headers map[string][]string, body []byte, timeout time.Duration) (int, []byte, error) {
	payload := map[string]any{
		"method":  method,
		"url":     target,
		"headers": headers,
	}
	if len(body) > 0 {
		payload["body"] = body // []byte → base64
	}
	raw, _ := json.Marshal(payload)

	type result struct {
		status int
		body   []byte
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		ret, err := callHost("host.http.do", raw)
		if err != nil {
			ch <- result{0, nil, err}
			return
		}
		var hr struct {
			StatusCode int    `json:"StatusCode"`
			Body       []byte `json:"Body"`
		}
		if err := json.Unmarshal(ret, &hr); err != nil {
			ch <- result{0, nil, err}
			return
		}
		ch <- result{hr.StatusCode, hr.Body, nil}
	}()
	select {
	case r := <-ch:
		return r.status, r.body, r.err
	case <-time.After(timeout):
		return 0, nil, fmt.Errorf("请求超时(%s)", timeout.String())
	}
}

func chatProbe(baseURL, key, model string, maxTokens int, timeout time.Duration) (int, []byte, error) {
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": maxTokens,
		"stream":     false,
	}
	body, _ := json.Marshal(payload)
	return httpDo("POST", baseURL+"/chat/completions", map[string][]string{
		"Authorization": {"Bearer " + key},
		"Content-Type":  {"application/json"},
	}, body, timeout)
}

// ------------------------- 配置解析 -------------------------

type providerInfo struct {
	Name     string
	BaseURL  string
	APIKeys  []string
	Disabled bool
}

type modelEntry struct {
	provider   string
	name       string
	alias      string
	public     string
	itemIdx    int
	lastKeyIdx int
	maxclIdx   int
	maxclVal   int
	scalar     bool
}

type parsedConfig struct {
	lines     []string
	providers map[string]*providerInfo
	order     []string
	models    []*modelEntry
}

var sectionRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:\s*(.*)$`)

func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func (pc *parsedConfig) modelsOf(provider string) []*modelEntry {
	var out []*modelEntry
	for _, m := range pc.models {
		if m.provider == provider {
			out = append(out, m)
		}
	}
	return out
}

func parseCoreConfig(text string) *parsedConfig {
	lines := splitLines(text)
	pc := &parsedConfig{lines: lines, providers: map[string]*providerInfo{}}
	section := ""
	var curProvider *providerInfo
	inModels := false
	cur := (*modelEntry)(nil)

	for i, raw := range lines {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))

		if indent == 0 {
			m := sectionRe.FindStringSubmatch(raw)
			if m != nil {
				section = m[1]
				if strings.HasPrefix(strings.TrimSpace(m[2]), "[") {
					section = ""
				}
			} else {
				section = ""
			}
			curProvider = nil
			inModels = false
			cur = nil
			continue
		}
		if section != "openai-compatibility" {
			continue
		}

		if strings.HasPrefix(s, "- ") {
			body := strings.TrimSpace(s[2:])
			if body == "" {
				continue
			}
			if indent <= 2 {
				// 供应商项
				cur = nil
				inModels = false
				if kv := keyValueRe.FindStringSubmatch(body); kv != nil && strings.EqualFold(kv[1], "name") {
					nm := unquote(kv[2])
					if existing, ok := pc.providers[nm]; ok {
						curProvider = existing
					} else {
						curProvider = &providerInfo{Name: nm}
						pc.providers[nm] = curProvider
						pc.order = append(pc.order, nm)
					}
				} else {
					curProvider = nil
				}
				continue
			}
			if curProvider != nil && indent == 6 {
				if inModels {
					cur = &modelEntry{provider: curProvider.Name, itemIdx: i, maxclIdx: -1}
					if kv := keyValueRe.FindStringSubmatch(body); kv != nil && strings.EqualFold(kv[1], "name") {
						cur.name = unquote(kv[2])
					} else {
						// 裸标量模型行
						cur.name = unquote(body)
						cur.scalar = true
					}
					cur.public = firstNonEmpty(cur.alias, cur.name)
					pc.models = append(pc.models, cur)
					continue
				}
				if kv := keyValueRe.FindStringSubmatch(body); kv != nil && strings.EqualFold(kv[1], "api-key") {
					curProvider.APIKeys = append(curProvider.APIKeys, unquote(kv[2]))
				}
				continue
			}
			continue
		}

		// 键行
		kv := keyValueRe.FindStringSubmatch(s)
		if kv == nil {
			continue
		}
		key := strings.ToLower(unquote(kv[1]))
		val := unquote(kv[2])
		if curProvider != nil && cur == nil && indent <= 4 {
			switch key {
			case "base-url":
				curProvider.BaseURL = val
			case "disabled":
				curProvider.Disabled = strings.EqualFold(val, "true")
			case "models":
				inModels = true
			case "api-key-entries":
				inModels = false
			}
			continue
		}
		if cur != nil && indent >= 8 {
			switch key {
			case "name":
				cur.name = val
				cur.public = firstNonEmpty(cur.alias, val)
			case "alias":
				cur.alias = val
				cur.public = firstNonEmpty(val, cur.name)
			case "max-context-length":
				cur.maxclIdx = i
				cur.maxclVal, _ = strconv.Atoi(val)
				cur.lastKeyIdx = i
			}
			cur.lastKeyIdx = i
			continue
		}
	}

	for _, m := range pc.models {
		m.public = firstNonEmpty(m.alias, m.name)
	}

	// 二次扫描：在供应商条目范围内查找 disabled: true（容忍任意缩进/位置）
	curP := (*providerInfo)(nil)
	for _, raw := range lines {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if indent <= 2 && strings.HasPrefix(s, "- ") {
			body := strings.TrimSpace(s[2:])
			if kv := keyValueRe.FindStringSubmatch(body); kv != nil && strings.EqualFold(kv[1], "name") {
				curP = pc.providers[unquote(kv[2])]
			}
			continue
		}
		if curP != nil {
			if kv := keyValueRe.FindStringSubmatch(s); kv != nil &&
				strings.EqualFold(strings.TrimSpace(unquote(kv[1])), "disabled") {
				switch strings.ToLower(unquote(kv[2])) {
				case "true", "1", "yes", "y", "on":
					curP.Disabled = true
				}
			}
		}
	}
	return pc
}

// ------------------------- 元数据 -------------------------

type metaLimits struct {
	models map[string]metaModel
}

type metaModel struct {
	ctx, out int
}

func fetchMetadata(baseURL, key string, timeout time.Duration) *metaLimits {
	st, body, err := httpDo("GET", baseURL+"/models", map[string][]string{
		"Authorization": {"Bearer " + key},
	}, nil, timeout)
	if err != nil || st != 200 {
		return nil
	}
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	ml := &metaLimits{models: map[string]metaModel{}}
	for _, m := range doc.Data {
		id, _ := m["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		mm := metaModel{}
		walkLimits(m, "", &mm)
		ml.models[id] = mm
		// 也按去前缀 id 索引
		if sl := strings.TrimPrefix(id, "/"); sl != id && strings.Contains(sl, "/") {
			short := sl[strings.LastIndex(sl, "/")+1:]
			if _, exists := ml.models[short]; !exists {
				ml.models[short] = mm
			}
		}
	}
	return ml
}

func walkLimits(m map[string]any, prefix string, mm *metaModel) {
	for k, v := range m {
		lk := strings.ToLower(k)
		switch vv := v.(type) {
		case map[string]any:
			walkLimits(vv, prefix+lk+".", mm)
		case float64:
			n := int(vv)
			if n < 1024 || n > 10000000 {
				continue
			}
			base := lk
			if idx := strings.LastIndex(base, "."); idx >= 0 {
				base = base[idx+1:]
			}
			if strings.Contains(base, "output") || strings.Contains(base, "completion") {
				if n > mm.out {
					mm.out = n
				}
			} else if strings.Contains(base, "context") || strings.Contains(base, "input") ||
				strings.Contains(base, "model_len") || strings.Contains(base, "window") || base == "max_tokens" || base == "maxtokens" {
				if n > mm.ctx {
					mm.ctx = n
				}
			}
		}
		_ = prefix
	}
}

func matchMetadata(meta *metaLimits, model string) (int, int, bool) {
	if meta == nil {
		return 0, 0, false
	}
	cands := []string{model}
	if sl := strings.TrimPrefix(model, "/"); sl != model && strings.Contains(sl, "/") {
		cands = append(cands, sl[strings.LastIndex(sl, "/")+1:])
	}
	for _, c := range cands {
		if mm, ok := meta.models[c]; ok {
			return mm.ctx, mm.out, true
		}
	}
	return 0, 0, false
}

// ------------------------- 探测单个模型 -------------------------

func probeModel(baseURL, key string, me *modelEntry, meta *metaLimits, mapping map[string]mapVal, timeout time.Duration) *modelResult {
	r := &modelResult{Provider: me.provider, Name: me.name, Public: me.public, Status: "unknown"}
	detail := []string{}

	claimedCtx := me.maxclVal
	if claimedCtx <= 0 {
		if mv, ok := mapping[strings.ToLower(me.public)]; ok && mv.Ctx > 0 {
			claimedCtx = mv.Ctx
		}
	}
	claimedOut := 0
	if mv, ok := mapping[strings.ToLower(me.public)]; ok {
		claimedOut = mv.Out
	}

	// 元数据（强证据）
	if ctx, out, ok := matchMetadata(meta, me.name); ok {
		if ctx > 0 {
			r.Context, r.SrcCtx = ctx, "元数据"
		}
		if out > 0 {
			r.Output, r.SrcOut = out, "元数据"
		}
		detail = append(detail, "元数据: /models 命中")
	}

	if cfg.MetadataOnly {
		if r.Context > 0 || r.Output > 0 {
			r.Status = "ok"
		} else {
			r.Status = "unknown"
			detail = append(detail, "元数据未给出上限")
		}
		r.Detail = detail
		return r
	}

	probeCtx := claimedCtx
	if probeCtx <= 0 {
		probeCtx = 1000000 // 声称默认
	}
	st1, body1, err1 := chatProbe(baseURL, key, me.name, probeCtx, timeout)
	if err1 != nil {
		detail = append(detail, "probe1 请求失败: "+err1.Error())
	} else if st1 >= 200 && st1 < 300 {
		if claimedCtx > 0 {
			r.Context, r.SrcCtx = claimedCtx, "官方·已接受"
		} else {
			r.Context, r.SrcCtx = probeCtx, "官方·已接受"
		}
		detail = append(detail, fmt.Sprintf("probe1: max_tokens=%d 被接受", probeCtx))
		if claimedOut > 0 {
			st2, body2, err2 := chatProbe(baseURL, key, me.name, claimedOut, timeout)
			if err2 != nil {
				detail = append(detail, "probe2 请求失败: "+err2.Error())
			} else if st2 >= 200 && st2 < 300 {
				r.Output, r.SrcOut = claimedOut, "官方·已接受"
				detail = append(detail, fmt.Sprintf("probe2: max_tokens=%d 被接受", claimedOut))
			} else {
				msg := errorMessage(body2)
				if msg != "" {
					detail = append(detail, "probe2: HTTP "+strconv.Itoa(st2)+" "+msg)
				}
				if kind, val := extractLimitFromError(msg); val > 0 {
					if kind == "output" {
						r.Output, r.SrcOut = val, "报错提取"
					} else {
						r.Context, r.SrcCtx = val, "报错提取"
					}
				}
			}
		}
	} else {
		msg := errorMessage(body1)
		if msg != "" {
			detail = append(detail, "probe1: HTTP "+strconv.Itoa(st1)+" "+msg)
		}
		if dead, note := classifyFailure(st1, msg); dead {
			r.Status = "dead"
			r.Detail = append(detail, "死渠道: "+note)
			return r
		}
		if n := transientNote(st1, msg); n != "" {
			detail = append(detail, n)
		}
		if kind, val := extractLimitFromError(msg); val > 0 {
			if kind == "output" {
				r.Output, r.SrcOut = val, "报错提取"
			} else {
				r.Context, r.SrcCtx = val, "报错提取"
			}
			detail = append(detail, "从报错提取上限")
			if kind == "output" {
				// max_tokens 被拒 → 用修正后的输出上限复测上下文（无元数据上下文时）
				if r.Context == 0 {
					retry := probeCtx
					if val > 0 && retry > val {
						retry = val
					}
					if retry > 0 && retry != probeCtx {
						st2, body2, err2 := chatProbe(baseURL, key, me.name, retry, timeout)
						if err2 != nil {
							detail = append(detail, "probe2(修正) 请求失败: "+err2.Error())
						} else if st2 >= 200 && st2 < 300 {
							r.Context, r.SrcCtx = retry, "官方·已接受"
							detail = append(detail, fmt.Sprintf("probe2(修正): max_tokens=%d 被接受", retry))
						} else {
							m2 := errorMessage(body2)
							if k2, v2 := extractLimitFromError(m2); v2 > 0 && k2 == "context" {
								r.Context, r.SrcCtx = v2, "报错提取"
							}
						}
					}
				}
			}
			// 若 context 已定而 output 未知，补一测
			if kind == "context" && claimedOut > 0 {
				st2, body2, err2 := chatProbe(baseURL, key, me.name, claimedOut, timeout)
				if err2 != nil {
					detail = append(detail, "probe2 请求失败: "+err2.Error())
				} else if st2 >= 200 && st2 < 300 {
					r.Output, r.SrcOut = claimedOut, "官方·已接受"
				} else {
					m2 := errorMessage(body2)
					if k2, v2 := extractLimitFromError(m2); v2 > 0 {
						if k2 == "output" {
							r.Output, r.SrcOut = v2, "报错提取"
						} else {
							r.Context, r.SrcCtx = v2, "报错提取"
						}
					}
				}
			}
		}
	}

	if r.Status != "dead" {
		if r.Context > 0 || r.Output > 0 {
			r.Status = "ok"
		} else {
			r.Status = "unknown"
		}
	}
	r.Detail = detail
	return r
}

// ------------------------- 报错提取 / 失败分类 -------------------------

var (
	reMaxCtxLen = regexp.MustCompile(`(?i)maximum context length is ([0-9,]+)`)
	reCtxTokens = regexp.MustCompile(`(?i)context length[^0-9]{0,30}([0-9,]+)\s*tokens`)
	reGreater   = regexp.MustCompile(`(?i)must not be greater than ([0-9,]+)`)
	reExceed    = regexp.MustCompile(`(?i)exceed[s]?\s+(?:the\s+)?(?:maximum|(?:service\s+)?limit)[^0-9]{0,40}([0-9,]+)`)
	// less or equal to N / 不超过 N 等变体
	reLessEq = regexp.MustCompile(`(?i)(?:should be less or equal to|less than or equal to|less or equal to|no more than|at most)\s*([0-9,]+)`)
	// 合法范围 [1, N] / 范围[1,N]
	reRange     = regexp.MustCompile(`(?i)(?:range|范围)[^\]】]{0,30}\[1\s*,\s*([0-9,]+)\s*\]`)
	reOutTokens = regexp.MustCompile(`(?i)(?:output|completion)[^0-9]{0,30}([0-9,]+)\s*tokens`)
	// N output tokens（数字在前，如 exceeds the service limit of 262144 output tokens）
	reNumOutTokens = regexp.MustCompile(`(?i)([0-9,]+)\s+(?:output|completion)\s+tokens`)
	// should be in [1, N]（如 field MaxTokens invalid, should be in [1, 131072]）
	reIn        = regexp.MustCompile(`(?i)\bin\s*\[1\s*,\s*([0-9,]+)\s*\]`)
	reAnyTokens = regexp.MustCompile(`(?i)max(?:imum)?[^0-9]{0,20}([0-9,]+)\s*tokens`)
)

func num(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}

func extractLimitFromError(msg string) (string, int) {
	if m := reMaxCtxLen.FindStringSubmatch(msg); m != nil {
		return "context", num(m[1])
	}
	if m := reGreater.FindStringSubmatch(msg); m != nil {
		v := num(m[1])
		lm := strings.ToLower(msg)
		if strings.Contains(lm, "output") || strings.Contains(lm, "completion") {
			return "output", v
		}
		return "context", v
	}
	if m := reExceed.FindStringSubmatch(msg); m != nil {
		v := num(m[1])
		lm := strings.ToLower(msg)
		if strings.Contains(lm, "output") || strings.Contains(lm, "completion") {
			return "output", v
		}
		return "context", v
	}
	// max_tokens 合法范围 [1, N]（如 valid range of max_tokens is [1, 393216]、限制数值范围[1,131072]）
	if m := reRange.FindStringSubmatch(msg); m != nil {
		lm := strings.ToLower(msg)
		if strings.Contains(lm, "context") || strings.Contains(lm, "input") {
			return "context", num(m[1])
		}
		return "output", num(m[1])
	}
	// should be in [1, N]（如 field MaxTokens invalid, should be in [1, 131072]）
	if m := reIn.FindStringSubmatch(msg); m != nil {
		lm := strings.ToLower(msg)
		if strings.Contains(lm, "context") || strings.Contains(lm, "input") {
			return "context", num(m[1])
		}
		return "output", num(m[1])
	}
	// should be less or equal to N 等
	if m := reLessEq.FindStringSubmatch(msg); m != nil {
		v := num(m[1])
		lm := strings.ToLower(msg)
		if strings.Contains(lm, "context") || strings.Contains(lm, "input") {
			return "context", v
		}
		return "output", v // 默认 max_tokens 类限制为输出上限
	}
	if m := reCtxTokens.FindStringSubmatch(msg); m != nil {
		return "context", num(m[1])
	}
	if m := reOutTokens.FindStringSubmatch(msg); m != nil {
		return "output", num(m[1])
	}
	// N output tokens（如 exceeds the service limit of 262144 output tokens）
	if m := reNumOutTokens.FindStringSubmatch(msg); m != nil {
		return "output", num(m[1])
	}
	if m := reAnyTokens.FindStringSubmatch(msg); m != nil {
		return "context", num(m[1])
	}
	return "", 0
}

// 临时性失败提示（可稍后单点重试）
func transientNote(status int, msg string) string {
	lm := strings.ToLower(msg)
	switch {
	case status == 429 || strings.Contains(lm, "concurrency limit") || strings.Contains(lm, "rate limit"):
		return "临时限流/并发上限，可稍后对该模型单点重试"
	case status == 502 || status == 503 || status == 504:
		return "网关暂时不可用，可稍后对该模型单点重试"
	}
	return ""
}

func classifyFailure(status int, msg string) (bool, string) {
	lm := strings.ToLower(msg)
	if status == 401 {
		return true, "认证失败(401)"
	}
	if status == 403 {
		return true, "权限/余额受限(403)"
	}
	if status == 404 {
		return true, "接口或模型不存在(404)"
	}
	if strings.Contains(lm, "model not found") || strings.Contains(lm, "no channel candidates") {
		return true, "模型不存在/无可用渠道"
	}
	if strings.Contains(lm, "insufficient") || strings.Contains(lm, "quota") ||
		strings.Contains(lm, "余额") || strings.Contains(lm, "không đủ") {
		return true, "余额不足"
	}
	if status >= 500 && strings.Contains(lm, "forbidden") {
		return true, "上游拒绝访问"
	}
	return false, ""
}

func errorMessage(body []byte) string {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err == nil {
		if e, ok := doc["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				return m
			}
		}
		if m, ok := doc["message"].(string); ok && m != "" {
			return m
		}
		if m, ok := doc["detail"].(string); ok && m != "" {
			return m
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ------------------------- 映射表（未校验渠道回落） -------------------------

type mapVal struct {
	Ctx, Out int
}

func loadMapping(path string) map[string]mapVal {
	m := map[string]mapVal{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return m
	}
	for k, v := range doc {
		if strings.HasPrefix(k, "_") {
			continue
		}
		mv := mapVal{}
		switch vv := v.(type) {
		case float64:
			mv.Ctx = int(vv)
		case map[string]any:
			if f, ok := vv["context"].(float64); ok {
				mv.Ctx = int(f)
			}
			if f, ok := vv["output"].(float64); ok {
				mv.Out = int(f)
			}
		}
		if mv.Ctx > 0 || mv.Out > 0 {
			m[strings.ToLower(k)] = mv
		}
	}
	return m
}

// ------------------------- payload.override（输出上限写回） -------------------------

type outNeed struct {
	prov string
	pub  string
	val  int
	tag  string
}

func yIndent(n int) string { return strings.Repeat(" ", n) }

// overrideWrites 依据探测出的输出上限生成 payload.override 追加规则（max_tokens）。
// 只追加、不改动既有规则（宿主 last-write-wins，后置规则生效），避免破坏手工特例。
func overrideWrites(lines []string, needs []outNeed) ([]edit, []string) {
	if len(needs) == 0 {
		return nil, nil
	}
	type ovRule struct {
		names map[string]bool
		val   int
	}
	rules := []ovRule{}
	payloadIdx, ovIdx, ovIndent := -1, -1, 2
	for i, l := range lines {
		if l == "" || l[0] == ' ' || l[0] == '\t' || l[0] == '#' {
			continue
		}
		if strings.HasPrefix(l, "payload:") {
			payloadIdx = i
			break
		}
	}
	secEnd := len(lines)
	if payloadIdx >= 0 {
		for i := payloadIdx + 1; i < len(lines); i++ {
			l := lines[i]
			if l == "" || l[0] == '#' {
				continue
			}
			if l[0] != ' ' && l[0] != '\t' {
				secEnd = i
				break
			}
		}
	}
	reOv := regexp.MustCompile(`^"?override"?:`)
	if payloadIdx >= 0 {
		for i := payloadIdx + 1; i < secEnd; i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			ind := len(l) - len(strings.TrimLeft(l, " \t"))
			if ind == 0 {
				break
			}
			if ovIdx < 0 && reOv.MatchString(strings.TrimLeft(l, " \t")) {
				ovIdx = i
				ovIndent = ind
			}
		}
	}
	var starts []int
	if ovIdx >= 0 {
		reRule := regexp.MustCompile(`^-\s+"?models"?:`)
		for i := ovIdx + 1; i < secEnd; i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			ind := len(l) - len(strings.TrimLeft(l, " \t"))
			if ind <= ovIndent {
				break
			}
			if ind == ovIndent+2 && reRule.MatchString(strings.TrimLeft(l, " \t")) {
				starts = append(starts, i)
			}
		}
		reName := regexp.MustCompile(`^-\s+"?name"?:\s*(.+?)\s*$`)
		reMT := regexp.MustCompile(`"?max_tokens"?:\s*([0-9,]+)`)
		for ri, s := range starts {
			end := secEnd
			if ri+1 < len(starts) {
				end = starts[ri+1]
			}
			names := map[string]bool{}
			val := 0
			for i := s; i < end; i++ {
				l := lines[i]
				ind := len(l) - len(strings.TrimLeft(l, " \t"))
				trimmed := strings.TrimLeft(l, " \t")
				if ind == ovIndent+6 {
					if m := reName.FindStringSubmatch(trimmed); m != nil {
						names[unquote(m[1])] = true
					}
				}
				if m := reMT.FindStringSubmatch(l); m != nil && val == 0 {
					val = num(m[1])
				}
			}
			rules = append(rules, ovRule{names: names, val: val})
		}
	}

	// 追加位置：最后一条规则末尾 / override 键后 / payload 段尾 / EOF
	insertAt := len(lines)
	var head []string
	switch {
	case ovIdx >= 0 && len(starts) > 0:
		last := starts[len(starts)-1]
		insertAt = secEnd
		for i := last + 1; i < secEnd; i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			ind := len(l) - len(strings.TrimLeft(l, " \t"))
			if ind <= ovIndent {
				insertAt = i
				break
			}
		}
	case ovIdx >= 0:
		insertAt = ovIdx + 1
	case payloadIdx >= 0:
		insertAt = secEnd
		head = []string{yIndent(ovIndent) + `"override":`}
	default:
		insertAt = len(lines)
		head = []string{"", "payload:", yIndent(ovIndent) + `"override":`}
	}

	// 已被同名同值规则覆盖的跳过，其余按值分组（一组一条规则，与既有风格一致）
	byVal := map[int][]outNeed{}
	for _, n := range needs {
		covered := false
		for _, r := range rules {
			if r.names[n.pub] && r.val == n.val {
				covered = true
				break
			}
		}
		if !covered {
			byVal[n.val] = append(byVal[n.val], n)
		}
	}
	if len(byVal) == 0 {
		return nil, nil
	}
	vals := make([]int, 0, len(byVal))
	for v := range byVal {
		vals = append(vals, v)
	}
	sort.Ints(vals)

	ruleIndent, paramIndent, nameIndent := 4, 6, 8
	if ovIdx >= 0 {
		ruleIndent = ovIndent + 2
		paramIndent = ovIndent + 4
		nameIndent = ovIndent + 6
	}
	var block []string
	var changes []string
	for _, v := range vals {
		items := byVal[v]
		sort.Slice(items, func(i, j int) bool { return items[i].pub < items[j].pub })
		block = append(block, yIndent(ruleIndent)+`- "models":`)
		for _, p := range items {
			name := strings.ReplaceAll(p.pub, `"`, `\"`)
			block = append(block,
				yIndent(nameIndent)+`- "name": "`+name+`"`,
				yIndent(nameIndent+2)+`"protocol": "openai"`,
				yIndent(nameIndent+2)+`"headers": {}`,
				yIndent(nameIndent+2)+`"from-protocol": ""`,
				yIndent(nameIndent+2)+`"match": []`,
				yIndent(nameIndent+2)+`"not-match": []`,
				yIndent(nameIndent+2)+`"exist": []`,
				yIndent(nameIndent+2)+`"not-exist": []`)
			changes = append(changes, "payload.override: "+p.prov+"/"+p.pub+" max_tokens→"+strconv.Itoa(v)+" ["+p.tag+"]")
		}
		block = append(block,
			yIndent(paramIndent)+`"params":`,
			yIndent(paramIndent+2)+`"max_tokens": `+strconv.Itoa(v))
	}
	block = append(head, block...)
	return []edit{{idx: insertAt, op: "insert", text: strings.Join(block, "\n")}}, changes
}

// ------------------------- 写回 config.yaml -------------------------

type edit struct {
	idx  int
	op   string // insert / replace
	text string
}

func applyReport(rep *probeReport) ([]string, error) {
	configPath, err := resolveConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	crlf := strings.Contains(text, "\r\n")
	pc := parseCoreConfig(text)
	mapping := loadMapping(mappingPathFor(configPath))

	byKey := map[string]*modelEntry{}
	for _, me := range pc.models {
		byKey[me.provider+"\x00"+me.name] = me
		if me.public != me.name {
			byKey[me.provider+"\x00"+me.public] = me
		}
	}

	var edits []edit
	var changes []string

	// 输出上限收集 → payload.override（max_tokens）
	outSeen := map[string]int{}
	var outNeeds []outNeed
	addOut := func(prov, pub string, v int, tag string) {
		if v <= 0 {
			return
		}
		if _, seen := outSeen[pub]; seen {
			return
		}
		outSeen[pub] = v
		outNeeds = append(outNeeds, outNeed{prov: prov, pub: pub, val: v, tag: tag})
	}

	providers := make([]string, 0, len(rep.Providers))
	for p := range rep.Providers {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	for _, pname := range providers {
		models := rep.Providers[pname]
		names := make([]string, 0, len(models))
		for n := range models {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, pub := range names {
			r := models[pub]
			if r == nil || r.Status == "dead" {
				continue
			}
			me := byKey[pname+"\x00"+r.Name]
			if me == nil {
				me = byKey[pname+"\x00"+pub]
			}
			if me == nil || me.scalar {
				continue
			}

			// 输出上限 → payload.override（max_tokens）
			if r.Output > 0 && r.SrcOut != "" {
				addOut(pname, me.public, r.Output, r.SrcOut)
			}

			// 解析本次应写回的值 + 依据
			var val int
			var tag string
			exit := false
			switch {
			case r.SrcCtx == "元数据" || r.SrcCtx == "报错提取":
				val, tag = r.Context, "["+r.SrcCtx+"]"
				if val <= 0 {
					exit = true
				}
			case r.SrcCtx == "官方·已接受":
				if me.maxclVal > 0 {
					continue // 现值已验证接受，保留
				}
				if r.Context <= 0 {
					continue
				}
				val, tag = r.Context, "[官方·已接受]"
			default:
				// unknown / failed → 映射表回落
				if mv, ok := mapping[strings.ToLower(me.public)]; ok {
					if mv.Ctx > 0 {
						val, tag = mv.Ctx, "[映射回落]"
					} else {
						continue
					}
					addOut(pname, me.public, mv.Out, "[映射回落]")
				} else {
					continue
				}
			}
			if exit {
				continue
			}

			if me.maxclVal == val && me.maxclIdx >= 0 {
				continue // 已经是该值，无需改动
			}

			old := r.Name + " → "
			if me.maxclIdx >= 0 {
				line := pc.lines[me.maxclIdx]
				ci := strings.LastIndex(line, ":")
				prefix := line[:ci+1]
				newLine := prefix + " " + strconv.Itoa(val)
				edits = append(edits, edit{idx: me.maxclIdx, op: "replace", text: newLine})
				old = fmt.Sprintf("%d → %d", me.maxclVal, val)
			} else {
				insertAt := me.lastKeyIdx
				if insertAt < me.itemIdx {
					insertAt = me.itemIdx
				}
				indent := indentOfItem + 2
				newLine := strings.Repeat(" ", indent) + `"max-context-length": ` + strconv.Itoa(val)
				edits = append(edits, edit{idx: insertAt + 1, op: "insert", text: newLine})
				old = fmt.Sprintf("（缺省）→ %d", val)
			}
			changes = append(changes, pname+"/"+me.name+" "+old+" "+tag)
		}
	}

	// 输出上限 → payload.override 追加规则
	oe, och := overrideWrites(pc.lines, outNeeds)
	edits = append(edits, oe...)
	changes = append(changes, och...)

	if len(edits) == 0 {
		return changes, nil
	}

	// 备份
	_ = os.WriteFile(configPath+".bak.plugin."+time.Now().Format("20060102-150405"), raw, 0o644)

	// 从后往前应用
	sort.Slice(edits, func(i, j int) bool { return edits[i].idx > edits[j].idx })
	lines := pc.lines
	for _, e := range edits {
		if e.op == "insert" {
			lines = append(lines[:e.idx], append([]string{e.text}, lines[e.idx:]...)...)
		} else {
			lines[e.idx] = e.text
		}
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := os.WriteFile(configPath, []byte(out), 0o644); err != nil {
		return nil, err
	}
	return changes, nil
}

// ------------------------- 工具 -------------------------

const indentOfItem = 6

func unquote(s string) string {
	s = strings.TrimSpace(s)
	quoted := false
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		s = s[1 : len(s)-1]
		quoted = true
	}
	if !quoted {
		if idx := strings.Index(s, " #"); idx >= 0 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mustJSONIndent(v any) []byte {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return raw
}
