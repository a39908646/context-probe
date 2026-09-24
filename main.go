// context-probe —— CLIProxyAPI 标准动态库插件（Management API 能力）。
//
// 功能（v0.10 起大幅精简）：
//  1. 列出 config.yaml 中 openai-compatibility 渠道的全部模型；
//  2. 支持「仅看上下文为空」筛选；
//  3. 单点 / 批量手动填写上下文（max-context-length）与输出上限（payload.override 的 max_tokens）；
//  4. 保存后写回 config.yaml（宿主 file watcher 自动热加载，写前自动备份）；
//  5. 测试：直接以 config 中填写的上下文值作为 max_tokens 发一次 chat 请求，看接口报不报错。
//
// 不再做自动探测 / 报错提取 / 元数据推断，值完全由用户决定。
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

// pluginVersion 插件发布版本（唯一手写处）。
// 宿主规范：非空、不以 v 开头、匹配 ^[0-9][0-9A-Za-z.+-]*$（见 internal/pluginstore/registry.go）；
// 更新检测按点分整数逐段比较，故用纯数字点分（如 0.10.0）最稳。
// README 顶部「当前版本」行由构建脚本/测试自动同步，勿手改。
const pluginVersion = "0.10.0"

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
	mu.Lock()
	stopFlag = true
	mu.Unlock()
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
	ConfigPath  string
	Providers   string
	TimeoutSecs int
	DelaySecs   float64
}

// modelResult 单个模型的一次测试结果。
type modelResult struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Public    string `json:"public"`
	Context   int    `json:"context,omitempty"`
	Output    int    `json:"output,omitempty"`
	MaxTokens int    `json:"max-tokens,omitempty"`
	HTTP      int    `json:"http,omitempty"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// testRun 一次「按 config 值测试」的汇总。
type testRun struct {
	StartedAt string                             `json:"started-at"`
	Total     int                                `json:"total"`
	Done      int                                `json:"done"`
	Current   string                             `json:"current"`
	Results   map[string]map[string]*modelResult `json:"results"` // provider → public → 结果
}

// probeSel 记录用户在页面勾选的模型集合：provider → model(public 或 name) → true
// 经 ?sel= 查询参数（JSON 数组 [[provider,model],...]）提交。
type probeSel map[string]map[string]bool

// count 返回选中的模型总数（去重后的 name/public 键）。
func (s probeSel) count() int {
	n := 0
	for _, m := range s {
		n += len(m)
	}
	return n
}

// has 判断某供应商的某模型是否被选中（name 或 public 任一命中）。
func (s probeSel) has(provider, model string) bool {
	if s == nil {
		return false
	}
	if m, ok := s[provider]; ok && (m[model] || m[strings.ToLower(model)]) {
		return true
	}
	return false
}

// parseSel 解析 ?sel= 参数：JSON 二维数组 [ ["provider","model"], ... ]。
// 解析失败或空返回 nil（表示不限定、全量）。
func parseSel(raw string) probeSel {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var pairs [][2]string
	if err := json.Unmarshal([]byte(raw), &pairs); err != nil {
		return nil
	}
	if len(pairs) == 0 {
		return nil
	}
	s := probeSel{}
	for _, p := range pairs {
		if p[0] == "" || p[1] == "" {
			continue
		}
		if s[p[0]] == nil {
			s[p[0]] = map[string]bool{}
		}
		s[p[0]][p[1]] = true
		s[p[0]][strings.ToLower(p[1])] = true
	}
	if len(s) == 0 {
		return nil
	}
	return s
}

var (
	cfg      = pluginConfig{TimeoutSecs: 60, DelaySecs: 0.3}
	mu       sync.Mutex
	running  bool
	stopFlag bool
	last     *testRun
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
				"Description": "模型列表：手动填写上下文 / 输出上限，保存写回 config，并按 config 值测试连通性",
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
	return registrationResult(), nil
}

func registrationResult() []byte {
	return okEnvelope(map[string]any{
		"schema_version": rpcSchemaVersion,
		"metadata": map[string]any{
			"Name":             "context-probe",
			"Version":          pluginVersion,
			"Author":           "cloudwayne",
			"GitHubRepository": "https://github.com/a39908646/context-probe",
			"ConfigFields": []map[string]any{
				{"Name": "config_path", "Type": "string", "Description": "config.yaml 路径（默认自动探测 cpa-core/config.yaml）"},
				{"Name": "providers", "Type": "string", "Description": "供应商过滤（子串，逗号分隔，空=全部）"},
				{"Name": "probe_timeout_seconds", "Type": "int", "Description": "单请求超时秒数（默认 60）"},
				{"Name": "probe_delay_seconds", "Type": "float", "Description": "测试请求间隔秒数（默认 0.3）"},
			},
		},
		"capabilities": map[string]any{"management_api": true},
	})
}

var keyValueRe = regexp.MustCompile(`^"?([^":]+)"?\s*:\s*(.*)$`)

func applyPluginConfig(text string) {
	mu.Lock()
	defer mu.Unlock()
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
	q := func(k string) string {
		if req.Query == nil {
			return ""
		}
		if v, ok := req.Query[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	op := strings.ToLower(q("op"))
	isJSON := strings.EqualFold(q("format"), "json")

	switch op {
	case "", "status":
		if isJSON {
			return mgmtResponse(200, "application/json", mustJSONIndent(statusJSON())), nil
		}
		return mgmtResponse(200, "text/html; charset=utf-8", listHTML()), nil
	case "test":
		sel := parseSel(q("sel"))
		started := startTest(sel)
		if isJSON {
			return mgmtResponse(200, "application/json",
				mustJSONIndent(map[string]any{"ok": true, "started": started})), nil
		}
		return mgmtResponse(200, "text/html; charset=utf-8", listHTML()), nil
	case "save":
		var edits []modelEdit
		if raw := q("edits"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &edits)
		}
		changes, err := applyEdits(edits)
		if isJSON {
			if err != nil {
				return mgmtResponse(200, "application/json",
					mustJSONIndent(map[string]any{"ok": false, "error": err.Error()})), nil
			}
			return mgmtResponse(200, "application/json",
				mustJSONIndent(map[string]any{"ok": true, "changes": changes})), nil
		}
		var body string
		if err != nil {
			body = "<h1>写回失败</h1><p>" + html.EscapeString(err.Error()) + "</p><p><a href=\"?op=status\">返回</a></p>"
		} else {
			parts := []string{"<h1>写回完成</h1><ul>"}
			for _, c := range changes {
				parts = append(parts, "<li>"+html.EscapeString(c)+"</li>")
			}
			parts = append(parts, "</ul><p><a href=\"?op=status\">返回</a></p>")
			body = strings.Join(parts, "")
		}
		return mgmtResponse(200, "text/html; charset=utf-8", renderPage("写回结果", body)), nil
	case "report":
		mu.Lock()
		rep := last
		mu.Unlock()
		if rep == nil {
			return mgmtResponse(404, "application/json", []byte(`{"error":"尚无测试结果"}`)), nil
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

func statusJSON() map[string]any {
	mu.Lock()
	defer mu.Unlock()
	out := map[string]any{
		"running": running,
		"total":   0,
		"done":    0,
		"current": "",
		"results": map[string]map[string]*modelResult{},
	}
	if last != nil {
		out["total"] = last.Total
		out["done"] = last.Done
		out["current"] = last.Current
		out["results"] = last.Results
	}
	return out
}

// ------------------------- HTML 页面 -------------------------

const pageScript = `<script>
(function(){
function qa(s,root){return Array.prototype.slice.call((root||document).querySelectorAll(s));}
function el(id){return document.getElementById(id);}
function rowVisible(tr){return tr.style.display!=='none';}
function rowCtxEmpty(tr){var ic=tr.querySelector('.in-ctx');return !ic||(ic.value||'').trim()==='';}
// visSel 只返回「当前可见」的勾选框：筛选后全选/批量/测试都不会波及被隐藏的行
function visSel(){return qa('.cp-sel').filter(function(cb){var tr=cb.closest('tr.cp-row');return tr&&rowVisible(tr);});}
function selInfo(){
  var vis=visSel();
  var n=vis.filter(function(c){return c.checked;}).length;
  var info=el('cp-sel-info');
  if(info)info.innerHTML='已选 <b>'+n+'</b> 个模型';
  qa('.cp-prov').forEach(function(pc){
    var p=pc.dataset.prov;
    var cbs=vis.filter(function(c){return c.dataset.prov===p;});
    var on=cbs.filter(function(c){return c.checked;}).length;
    pc.checked=cbs.length>0&&on===cbs.length; pc.indeterminate=on>0&&on<cbs.length;
  });
}
function filterEmpty(){
  var on=el('cp-only-empty')&&el('cp-only-empty').checked;
  qa('tr.cp-row').forEach(function(tr){tr.style.display=(!on||rowCtxEmpty(tr))?'':'none';});
  // 供应商下没有可见模型时，整个表头/卡片一并隐藏
  qa('.prov-card').forEach(function(card){
    card.style.display=qa('tr.cp-row',card).some(rowVisible)?'':'none';
  });
  selInfo();
}
function toast(title,lines){
  var t=el('cp-toast'); if(!t)return;
  el('cp-toast-title').textContent=title||'';
  var u=el('cp-toast-list'); u.innerHTML='';
  (lines||[]).forEach(function(l){var li=document.createElement('li');li.textContent=l;u.appendChild(li);});
  el('cp-toast-empty').style.display=(lines&&lines.length)?'none':'block';
  t.classList.add('show');
}
function cpSave(){
  var edits=[];
  qa('tr.cp-row').forEach(function(tr){
    var ic=tr.querySelector('.in-ctx'), io=tr.querySelector('.in-out');
    if(!ic||ic.disabled)return;
    var cv=(ic.value||'').trim(), ov=(io.value||'').trim();
    if(cv===(tr.dataset.ctx||'')&&ov===(tr.dataset.out||''))return;
    edits.push({provider:tr.dataset.prov,name:tr.dataset.name,public:tr.dataset.pub,
      context:parseInt(cv||'0',10)||0, output:parseInt(ov||'0',10)||0});
  });
  if(!edits.length){toast('没有改动',['请先修改或批量填写上下文 / 输出上限']);return;}
  var btn=el('btn-save'); btn.disabled=true; var old=btn.textContent; btn.textContent='保存中…';
  fetch('?op=save&format=json&edits='+encodeURIComponent(JSON.stringify(edits)),{cache:'no-store'})
   .then(function(r){return r.json();}).then(function(d){
     btn.disabled=false; btn.textContent=old;
     if(d.ok){toast('写回完成',(d.changes&&d.changes.length)?d.changes:['无实际变更']);setTimeout(function(){location.reload();},900);}
     else{toast('写回失败',[d.error||'未知错误']);}
   }).catch(function(err){btn.disabled=false;btn.textContent=old;toast('写回失败',[String(err)]);});
}
function cpTest(){
  var vis=visSel();
  var checked=vis.filter(function(c){return c.checked;});
  var use=checked.length?checked:vis; // 未勾选时只测试当前可见模型
  var pairs=use.map(function(cb){return [cb.dataset.prov,cb.dataset.pub];});
  if(!pairs.length){toast('没有可测试的模型',['当前筛选下没有模型']);return;}
  var url='?op=test&format=json&sel='+encodeURIComponent(JSON.stringify(pairs));
  var btn=el('btn-test'); btn.disabled=true; var old=btn.textContent; btn.textContent='测试中…';
  fetch(url,{cache:'no-store'}).then(function(r){return r.json();}).then(function(d){
    if(!d.ok){btn.disabled=false;btn.textContent=old;toast('操作失败',[d.error||'未知错误']);return;}
    toast(d.started?'已开始测试':'测试已在运行中',['按 config 中的上下文值发起请求，请稍候']);
    cpPoll();
  }).catch(function(err){btn.disabled=false;btn.textContent=old;toast('操作失败',[String(err)]);});
}
function statusMeta(s){
  switch(s){
    case 'ok':return ['st-ok','可用'];
    case 'dead':return ['st-dead','死渠道'];
    case 'error':return ['st-dead','报错'];
    case 'net':return ['st-unknown','网络错误'];
    case 'skipped':return ['st-unknown','已跳过'];
    case 'pending':return ['st-pending','待测试'];
    default:return ['muted','—'];
  }
}
function cpApply(d){
  var rs=d.results||{};
  qa('tr.cp-row').forEach(function(tr){
    var r=(rs[tr.dataset.prov]||{})[tr.dataset.pub];
    if(!r)return;
    var st=tr.querySelector('.st-cell'), de=tr.querySelector('.detail-cell');
    var m=statusMeta(r.status);
    st.className='st-cell '+m[0]; st.textContent=m[1];
    de.textContent=r.detail||'';
  });
}
function cpPoll(){
  fetch('?op=status&format=json',{cache:'no-store'}).then(function(r){return r.json();}).then(function(d){
    cpApply(d);
    if(d.running){setTimeout(cpPoll,1500);return;}
    var btn=el('btn-test'); if(btn){btn.disabled=false;btn.textContent='▶ 按 config 值测试';}
  }).catch(function(){setTimeout(cpPoll,2000);});
}
document.addEventListener('change',function(e){
  var t=e.target;
  if(!t||!t.classList)return;
  if(t.classList.contains('cp-prov')){
    var p=t.dataset.prov;
    visSel().forEach(function(c){if(c.dataset.prov===p)c.checked=t.checked;});
    selInfo();return;
  }
  if(t.classList.contains('cp-sel')){selInfo();return;}
  if(t.id==='cp-only-empty'){filterEmpty();}
});
document.addEventListener('input',function(e){
  if(e.target&&e.target.classList&&e.target.classList.contains('in-ctx')){
    var f=el('cp-only-empty'); if(f&&f.checked)filterEmpty();
  }
});
document.addEventListener('click',function(e){
  var t=e.target; if(!t||!t.id)return;
  if(t.id==='cp-toast-close'){el('cp-toast').classList.remove('show');return;}
  if(t.id==='btn-sel-all'){visSel().forEach(function(c){if(!c.disabled)c.checked=true;});selInfo();return;}
  if(t.id==='btn-sel-none'){qa('.cp-sel').forEach(function(c){c.checked=false;});selInfo();return;}
  if(t.id==='btn-batch-ctx'||t.id==='btn-batch-out'){
    var isCtx=t.id==='btn-batch-ctx';
    var src=(el(isCtx?'batch-ctx':'batch-out').value||'').trim();
    if(!src){toast('未填写数值',['请先在批量输入框填写一个值']);return;}
    var n=0;
    visSel().filter(function(cb){return cb.checked;}).forEach(function(cb){
      var tr=cb.closest('tr.cp-row'); if(!tr)return;
      var inp=tr.querySelector(isCtx?'.in-ctx':'.in-out');
      if(inp&&!inp.disabled){inp.value=src;n++;}
    });
    toast('已应用到 '+n+' 个模型',['点击「保存并回写」写入 config.yaml']);
    return;
  }
  if(t.id==='btn-save'){cpSave();return;}
  if(t.id==='btn-test'){cpTest();return;}
});
selInfo();
if(document.getElementById('cp-running')){
  var b=el('btn-test'); if(b){b.disabled=true;b.textContent='测试中…';}
  cpPoll();
}
})();
</script>`

const pageStyle = `<style>
:root{--bg:#ffffff;--panel:#f6f8fa;--border:#d0d7de;--text:#1f2328;--muted:#656d76;
--accent:#0969da;--ok:#1a7f37;--dead:#cf222e;--warn:#9a6700;--row-alt:#f6f8fa;--th-bg:#f6f8fa;--chip-bg:#ffffff}
@media (prefers-color-scheme: dark){:root{--bg:#0d1117;--panel:#161b22;--border:#30363d;--text:#e6edf3;--muted:#8b949e;
--accent:#2f81f7;--ok:#3fb950;--dead:#f85149;--warn:#d29922;--row-alt:#11151c;--th-bg:#1c2128;--chip-bg:#0d1117}}
*{box-sizing:border-box}
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;margin:0;background:var(--bg);color:var(--text);font-size:14px;line-height:1.6}
.wrap{max-width:1100px;margin:0 auto;padding:24px 20px 64px}
h1{font-size:20px;margin:0 0 16px;font-weight:600}
h2{font-size:15px;margin:28px 0 8px;font-weight:600;border-bottom:1px solid var(--border);padding-bottom:6px}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
.btn{display:inline-block;padding:6px 14px;border-radius:6px;font-size:13px;font-weight:500;
border:1px solid var(--border);background:var(--panel);color:var(--text);cursor:pointer;transition:background .15s}
.btn:hover{background:var(--row-alt);text-decoration:none}
.btn:disabled{opacity:.55;cursor:not-allowed}
.btn-primary{background:var(--accent);border-color:var(--accent);color:#fff}
.btn-primary:hover{background:#1f6feb}
.inp{width:110px;padding:4px 6px;border:1px solid var(--border);border-radius:4px;background:var(--bg);color:var(--text);font-size:12px}
.toolbar{display:flex;gap:10px;align-items:center;margin-bottom:12px;flex-wrap:wrap}
.card{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:12px 16px;margin-bottom:14px}
.muted{color:var(--muted);font-size:12px}
.st-ok{color:var(--ok)}.st-dead{color:var(--dead)}.st-unknown{color:var(--warn)}.st-pending{color:var(--accent)}
table{border-collapse:collapse;width:100%;margin-top:4px;font-size:13px;table-layout:fixed}
th,td{border:1px solid var(--border);padding:5px 8px;text-align:left;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
th{background:var(--th-bg);font-weight:600}
td.wrap-cell{white-space:normal;word-break:break-all}
tr:nth-child(even) td{background:var(--row-alt)}
.mdl-table th:nth-child(1),.mdl-table td:nth-child(1){width:34px;text-align:center}
.mdl-table th:nth-child(2),.mdl-table td:nth-child(2){width:28%}
.mdl-table th:nth-child(3),.mdl-table td:nth-child(3){width:15%}
.mdl-table th:nth-child(4),.mdl-table td:nth-child(4){width:15%}
.mdl-table th:nth-child(5),.mdl-table td:nth-child(5){width:10%}
.mdl-table th:nth-child(6),.mdl-table td:nth-child(6){width:auto}
.mdl-table input[type=checkbox]{margin:0;width:14px;height:14px;vertical-align:middle;accent-color:var(--accent);cursor:pointer}
.prov-head{display:flex;align-items:center;gap:8px;margin:0 0 6px;font-size:14px;font-weight:600;flex-wrap:wrap}
.prov-label{display:inline-flex;align-items:center;gap:6px;cursor:pointer}
.prov-label input{margin:0;width:15px;height:15px;accent-color:var(--accent);cursor:pointer}
.prov-body{overflow-x:auto}
.toast{position:fixed;top:64px;right:16px;max-width:460px;background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:12px 16px;box-shadow:0 6px 24px rgba(0,0,0,.25);z-index:9999;display:none}
.toast.show{display:block}
.toast h3{margin:0 0 8px;font-size:14px;padding-right:24px}
.toast ul{max-height:320px;overflow:auto;padding-left:18px;margin:0;font-size:12px;color:var(--muted)}
.toast .empty{margin:0;font-size:12px;color:var(--muted)}
.toast-x{position:absolute;top:8px;right:10px;border:none;background:none;color:var(--muted);font-size:14px;cursor:pointer;line-height:1}
.toast-x:hover{color:var(--text)}
</style>`

const toastHTML = `<div id="cp-toast" class="toast"><button id="cp-toast-close" class="toast-x" title="关闭">✕</button><h3 id="cp-toast-title"></h3><ul id="cp-toast-list"></ul><p id="cp-toast-empty" class="empty">没有需要写回的变更</p></div>`

func renderPage(title, bodyHTML string) []byte {
	s := `<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><title>` + html.EscapeString(title) +
		`</title></head><body>` + pageScript + pageStyle + toastHTML + bodyHTML + "</body></html>"
	return []byte(s)
}

// listHTML 渲染单页：模型列表 + 筛选 + 批量编辑 + 测试。
func listHTML() []byte {
	mu.Lock()
	run := running
	rep := last
	mu.Unlock()

	var b strings.Builder
	b.WriteString(`<div class="wrap">`)
	b.WriteString("<h1>Context Probe · 模型列表</h1>")

	b.WriteString(`<div class="toolbar">` +
		`<button class="btn" id="btn-sel-all" type="button">全选</button>` +
		`<button class="btn" id="btn-sel-none" type="button">全不选</button>` +
		`<span class="muted" id="cp-sel-info">已选 <b>0</b> 个模型</span>` +
		`<label class="muted" style="display:inline-flex;align-items:center;gap:4px;cursor:pointer">` +
		`<input type="checkbox" id="cp-only-empty"> 仅看上下文为空</label>` +
		`</div>`)

	b.WriteString(`<div class="toolbar">` +
		`<span class="muted">批量上下文</span><input class="inp" id="batch-ctx" type="number" min="1" placeholder="如 128000">` +
		`<button class="btn" id="btn-batch-ctx" type="button">应用到已选</button>` +
		`<span class="muted">批量输出上限</span><input class="inp" id="batch-out" type="number" min="1" placeholder="如 4096">` +
		`<button class="btn" id="btn-batch-out" type="button">应用到已选</button>` +
		`<button class="btn btn-primary" id="btn-save" type="button">保存并回写 config</button>` +
		`<button class="btn" id="btn-test" type="button">▶ 按 config 值测试</button>` +
		`<a class="btn" href="?op=report" target="_blank">JSON 报告</a></div>`)

	b.WriteString(infoLine(run, rep))

	configPath, err := resolveConfigPath()
	if err != nil {
		b.WriteString(`<div class="card"><p class="muted">` + html.EscapeString(err.Error()) + `</p></div></div>`)
		return renderPage("context-probe · 模型列表", b.String())
	}
	raw, rerr := os.ReadFile(configPath)
	if rerr != nil {
		b.WriteString(`<div class="card"><p class="muted">读取 config.yaml 失败：` + html.EscapeString(rerr.Error()) + `</p></div></div>`)
		return renderPage("context-probe · 模型列表", b.String())
	}
	pc := parseCoreConfig(string(raw))
	outs := overrideOutputs(pc.lines)

	probeable := func(p *providerInfo) bool {
		return p != nil && !p.Disabled && p.BaseURL != "" && len(p.APIKeys) > 0
	}
	filter := strings.ToLower(cfg.Providers)

	anyModel := false
	for _, pname := range pc.order {
		p := pc.providers[pname]
		if !probeable(p) {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(pname), filter) &&
			!strings.Contains(strings.ToLower(p.BaseURL), filter) {
			continue
		}
		models := pc.modelsOf(pname)
		if len(models) == 0 {
			continue
		}
		anyModel = true
		pid := "prov-" + html.EscapeString(pname)
		b.WriteString(`<div class="card prov-card">`)
		b.WriteString(`<div class="prov-head">` +
			`<label class="prov-label"><input type="checkbox" class="cp-prov" data-prov="` + html.EscapeString(pname) + `" title="勾选/取消该供应商全部模型"> <b>` + html.EscapeString(pname) + `</b></label>` +
			`<span class="muted">` + html.EscapeString(p.BaseURL) + `</span>` +
			`<span class="muted">` + strconv.Itoa(len(models)) + ` 个模型</span></div>`)
		b.WriteString(`<div class="prov-body" id="` + pid + `">`)
		b.WriteString(`<table class="mdl-table"><thead><tr><th class="ck"></th><th>模型</th><th>上下文</th><th>输出上限</th><th>状态</th><th>详情</th></tr></thead><tbody>`)
		for _, me := range models {
			var res *modelResult
			if rep != nil {
				if pm, ok := rep.Results[pname]; ok {
					res = pm[me.public]
				}
			}
			b.WriteString(listRow(pname, me, outs[me.public], res))
		}
		b.WriteString(`</tbody></table></div></div>`)
	}
	if !anyModel {
		b.WriteString(`<div class="card"><p class="muted">没有可展示的供应商（需启用且配置 base-url 与 api-key；如设置了 providers 过滤请检查）。</p></div>`)
	}
	if run {
		b.WriteString(`<span id="cp-running" hidden></span>`)
	}
	b.WriteString(`</div>`)
	return renderPage("context-probe · 模型列表", b.String())
}

// listRow 渲染模型列表的一行：上下文/输出上限为可编辑输入框，状态/详情来自最近一次测试。
func listRow(pname string, me *modelEntry, out int, res *modelResult) string {
	ctx, outS := "", ""
	if me.maxclVal > 0 {
		ctx = strconv.Itoa(me.maxclVal)
	}
	if out > 0 {
		outS = strconv.Itoa(out)
	}
	stCls, stLabel, detail := "muted", "—", ""
	if res != nil {
		stCls, stLabel = statusLabel(res.Status)
		detail = res.Detail
	}
	dis := ""
	if me.scalar {
		dis = ` disabled title="裸标量模型行不支持写回，请在 config.yaml 中改为键值形式"`
	}
	prov := html.EscapeString(pname)
	name := html.EscapeString(me.name)
	pub := html.EscapeString(me.public)
	return `<tr class="cp-row" data-prov="` + prov + `" data-name="` + name + `" data-pub="` + pub +
		`" data-ctx="` + ctx + `" data-out="` + outS + `">` +
		`<td class="ck"><input type="checkbox" class="cp-sel" data-prov="` + prov + `" data-pub="` + pub + `" data-name="` + name + `" title="勾选后参与批量编辑 / 测试"></td>` +
		`<td>` + pub + `</td>` +
		`<td><input class="inp in-ctx" type="number" min="1" value="` + ctx + `" placeholder="空"` + dis + `></td>` +
		`<td><input class="inp in-out" type="number" min="1" value="` + outS + `" placeholder="空"` + dis + `></td>` +
		`<td class="st-cell ` + stCls + `">` + stLabel + `</td>` +
		`<td class="detail-cell wrap-cell muted">` + html.EscapeString(detail) + `</td></tr>`
}

// statusLabel 把状态码转为展示样式与文案。
func statusLabel(s string) (string, string) {
	switch s {
	case "ok":
		return "st-ok", "可用"
	case "dead":
		return "st-dead", "死渠道"
	case "error":
		return "st-dead", "报错"
	case "net":
		return "st-unknown", "网络错误"
	case "skipped":
		return "st-unknown", "已跳过"
	case "pending":
		return "st-pending", "待测试"
	default:
		return "muted", "—"
	}
}

// infoLine 渲染配置摘要 / 测试进度。
func infoLine(run bool, rep *testRun) string {
	s := `<p class="muted">config_path=` + html.EscapeString(cfg.ConfigPath) +
		" · providers=[" + html.EscapeString(cfg.Providers) + "]" +
		" · timeout=" + strconv.Itoa(cfg.TimeoutSecs) + "s" +
		" · delay=" + strconv.FormatFloat(cfg.DelaySecs, 'f', -1, 64) + "s"
	if run && rep != nil {
		s += ` · <b>测试中 ` + strconv.Itoa(rep.Done) + "/" + strconv.Itoa(rep.Total) + `</b>`
		if rep.Current != "" {
			s += "（" + html.EscapeString(rep.Current) + "）"
		}
	} else if rep != nil {
		s += " · 上次测试：" + html.EscapeString(rep.StartedAt)
	}
	return s + "</p>"
}

// ------------------------- 测试引擎 -------------------------

func startTest(sel probeSel) bool {
	mu.Lock()
	defer mu.Unlock()
	if running {
		return false
	}
	running = true
	stopFlag = false
	go runTest(sel)
	return true
}

func nowStr() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

// runTest 直接以 config 中填写的上下文值作为 max_tokens 发一次 chat 请求，记录是否被接受。
func runTest(sel probeSel) {
	defer func() {
		mu.Lock()
		if last != nil {
			last.Current = ""
		}
		running = false
		mu.Unlock()
	}()

	configPath, err := resolveConfigPath()
	if err != nil {
		return
	}
	raw, rerr := os.ReadFile(configPath)
	if rerr != nil {
		return
	}
	pc := parseCoreConfig(string(raw))
	outs := overrideOutputs(pc.lines)

	timeout := time.Duration(cfg.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	delay := time.Duration(cfg.DelaySecs * float64(time.Second))
	filter := strings.ToLower(cfg.Providers)
	selMode := len(sel) > 0

	type target struct {
		pname string
		p     *providerInfo
		me    *modelEntry
	}
	var targets []target
	for _, pname := range pc.order {
		p := pc.providers[pname]
		if p == nil || p.Disabled || p.BaseURL == "" || len(p.APIKeys) == 0 {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(pname), filter) &&
			!strings.Contains(strings.ToLower(p.BaseURL), filter) {
			continue
		}
		for _, me := range pc.modelsOf(pname) {
			if selMode && !sel.has(pname, me.name) && !sel.has(pname, me.public) {
				continue
			}
			targets = append(targets, target{pname, p, me})
		}
	}

	run := &testRun{StartedAt: nowStr(), Total: len(targets), Results: map[string]map[string]*modelResult{}}
	mu.Lock()
	last = run
	mu.Unlock()

	for _, tg := range targets {
		if stopped() {
			break
		}
		r := &modelResult{
			Provider: tg.pname,
			Name:     tg.me.name,
			Public:   tg.me.public,
			Context:  tg.me.maxclVal,
			Output:   outs[tg.me.public],
		}
		if tg.me.maxclVal <= 0 {
			r.Status, r.Detail = "skipped", "config 未填写上下文，已跳过（请先填写并保存）"
		} else {
			r.MaxTokens = tg.me.maxclVal
			baseURL := strings.TrimRight(tg.p.BaseURL, "/")
			status, body, derr := chatProbeWithRetry(baseURL, tg.p.APIKeys[0], tg.me.name,
				providerHeaders(tg.p), tg.me.maxclVal, timeout)
			switch {
			case derr != nil:
				r.Status, r.Detail = "net", derr.Error()
			case status >= 200 && status < 300:
				r.Status = "ok"
				r.Detail = fmt.Sprintf("HTTP %d：max_tokens=%d 被接受", status, tg.me.maxclVal)
			default:
				msg := errorMessage(body)
				r.HTTP = status
				if dead, note := classifyFailure(status, msg); dead {
					r.Status, r.Detail = "dead", note+"："+firstN(msg, 200)
				} else {
					r.Status = "error"
					r.Detail = fmt.Sprintf("HTTP %d：%s", status, firstN(msg, 200))
				}
			}
		}
		mu.Lock()
		if run.Results[tg.pname] == nil {
			run.Results[tg.pname] = map[string]*modelResult{}
		}
		run.Results[tg.pname][tg.me.public] = r
		run.Done++
		run.Current = tg.pname + "/" + tg.me.name
		mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

func stopped() bool {
	mu.Lock()
	defer mu.Unlock()
	return stopFlag
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

// providerHeaders 把供应商配置的自定义请求头转为测试请求用 map；
// $ 前缀的动态值（宿主从下游客户端请求复制）测试时无法还原，跳过。
func providerHeaders(p *providerInfo) map[string][]string {
	out := map[string][]string{}
	if p == nil {
		return out
	}
	for k, v := range p.Headers {
		if k == "" || strings.HasPrefix(v, "$") {
			continue
		}
		out[k] = []string{v}
	}
	return out
}

// chatProbeContent 对 /chat/completions 发一次非流式请求，max_tokens 为待验证的上下文值。
func chatProbeContent(baseURL, key, model string, headers map[string][]string, content string, maxTokens int, timeout time.Duration) (int, []byte, error) {
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": content}},
		"max_tokens": maxTokens,
		"stream":     false,
	}
	body, _ := json.Marshal(payload)
	// 与宿主 openai_compat_executor 一致：先设默认头，再用供应商自定义头覆盖（含 UA 可被覆盖）
	hdrs := map[string][]string{
		"Authorization": {"Bearer " + key},
		"Content-Type":  {"application/json"},
		"User-Agent":    {"cli-proxy-openai-compat"},
	}
	for k, v := range headers {
		hdrs[k] = v
	}
	return httpDo("POST", baseURL+"/chat/completions", hdrs, body, timeout)
}

// chatProbeWithRetry 对传输层失败（EOF / connection reset / 超时等，服务端直接断连，
// 常见为瞬时问题）自动重试有限次，避免单次抖动把模型误判为不可用。
func chatProbeWithRetry(baseURL, key, model string, headers map[string][]string, maxTokens int, timeout time.Duration) (int, []byte, error) {
	var st int
	var body []byte
	var err error
	const attempts = 3
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 2 * time.Second) // 2s / 4s 递增退避
		}
		st, body, err = chatProbeContent(baseURL, key, model, headers, "hi", maxTokens, timeout)
		if err == nil {
			return st, body, nil
		}
		if !isTransientNetErr(err) {
			return st, body, err // 非瞬时错误（如 URL 非法）不重试
		}
	}
	return st, body, err
}

var reNetTransient = regexp.MustCompile(`EOF|connection reset|broken pipe|connection refused|tls:|timeout|context deadline|no such host|temporary failure|i/o timeout|server closed idle connection`)

func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	return reNetTransient.MatchString(strings.ToLower(err.Error()))
}

// firstN 截断过长文本用于详情显示。
func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ------------------------- 失败分类 / 报错文本 -------------------------

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
		strings.Contains(lm, "余额") {
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

// ------------------------- 配置解析 -------------------------

type providerInfo struct {
	Name     string
	BaseURL  string
	APIKeys  []string
	Disabled bool
	// Headers 供应商配置的自定义请求头（headers: 键，静态值；$ 前缀动态值不在此还原）
	Headers map[string]string
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
	inHeaders := false
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
				inHeaders = false
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
				// 列表项（模型 / api-key）：结束 headers 块
				inHeaders = false
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
		rawKey := unquote(kv[1])
		if curProvider != nil && indent <= 4 {
			// 供应商级键（可能在 models 块之后，如 prefix/headers）：重置当前模型
			cur = nil
			inHeaders = false
			switch key {
			case "base-url":
				curProvider.BaseURL = val
			case "disabled":
				curProvider.Disabled = strings.EqualFold(val, "true")
			case "models":
				inModels = true
			case "api-key-entries":
				inModels = false
			case "headers":
				inHeaders = true
				if curProvider.Headers == nil {
					curProvider.Headers = map[string]string{}
				}
			}
			continue
		}
		if inHeaders && curProvider != nil && cur == nil && indent >= 6 {
			// headers 块内的键值对：保留原始大小写；$ 前缀动态值原样保留，由 providerHeaders 在请求时过滤
			if rawKey != "" {
				if curProvider.Headers == nil {
					curProvider.Headers = map[string]string{}
				}
				curProvider.Headers[rawKey] = val
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

// ------------------------- 写回 config.yaml -------------------------

type edit struct {
	idx  int
	op   string // insert / replace
	text string
}

// modelEdit 前端提交的一次手动编辑（context / output 为 0 表示不改该项）。
type modelEdit struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Public   string `json:"public"`
	Context  int    `json:"context"`
	Output   int    `json:"output"`
}

// applyEdits 把手动编辑写回 config.yaml：
//   - context → 模型项内联 max-context-length；
//   - output  → payload.override 追加 max_tokens 规则（同名同值跳过）。
func applyEdits(edits []modelEdit) ([]string, error) {
	if len(edits) == 0 {
		return nil, nil
	}
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

	byKey := map[string]*modelEntry{}
	for _, me := range pc.models {
		byKey[me.provider+"\x00"+me.name] = me
		if me.public != me.name {
			byKey[me.provider+"\x00"+me.public] = me
		}
	}

	var lineEdits []edit
	var changes []string
	var outNeeds []outNeed

	for _, e := range edits {
		me := byKey[e.Provider+"\x00"+e.Name]
		if me == nil {
			me = byKey[e.Provider+"\x00"+e.Public]
		}
		if me == nil || me.scalar {
			continue
		}
		if e.Context > 0 && me.maxclVal != e.Context {
			if me.maxclIdx >= 0 {
				line := pc.lines[me.maxclIdx]
				ci := strings.LastIndex(line, ":")
				lineEdits = append(lineEdits, edit{idx: me.maxclIdx, op: "replace",
					text: line[:ci+1] + " " + strconv.Itoa(e.Context)})
				changes = append(changes, fmt.Sprintf("%s/%s max-context-length %d → %d",
					me.provider, me.name, me.maxclVal, e.Context))
			} else {
				insertAt := me.lastKeyIdx
				if insertAt < me.itemIdx {
					insertAt = me.itemIdx
				}
				newLine := strings.Repeat(" ", indentOfItem+2) + `"max-context-length": ` + strconv.Itoa(e.Context)
				lineEdits = append(lineEdits, edit{idx: insertAt + 1, op: "insert", text: newLine})
				changes = append(changes, fmt.Sprintf("%s/%s max-context-length （缺省）→ %d",
					me.provider, me.name, e.Context))
			}
		}
		if e.Output > 0 {
			outNeeds = append(outNeeds, outNeed{prov: e.Provider, pub: me.public, val: e.Output, tag: "手动设置"})
		}
	}

	oe, och := overrideWrites(pc.lines, outNeeds)
	lineEdits = append(lineEdits, oe...)
	changes = append(changes, och...)

	if len(lineEdits) == 0 {
		return changes, nil
	}

	_ = os.WriteFile(configPath+".bak.plugin."+time.Now().Format("20060102-150405"), raw, 0o644)

	sort.Slice(lineEdits, func(i, j int) bool { return lineEdits[i].idx > lineEdits[j].idx })
	lines := pc.lines
	for _, e := range lineEdits {
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

// ------------------------- payload.override（输出上限） -------------------------

type outNeed struct {
	prov string
	pub  string
	val  int
	tag  string
}

func yIndent(n int) string { return strings.Repeat(" ", n) }

func num(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}

var (
	reOverrideKey = regexp.MustCompile(`^"?override"?:`)
	reRuleStart   = regexp.MustCompile(`^-\s+"?models"?:`)
	reNameAny     = regexp.MustCompile(`"?name"?\s*:\s*("[^"]*"|[^,\s}\]]+)`)
	reMaxTokens   = regexp.MustCompile(`"?max_tokens"?\s*:\s*([0-9,]+)`)
)

// overrideOutputs 从 payload.override 规则中读取模型名 → max_tokens 映射（后出现者覆盖前者）。
func overrideOutputs(lines []string) map[string]int {
	out := map[string]int{}
	payloadIdx := -1
	for i, l := range lines {
		if l != "" && l[0] != ' ' && l[0] != '\t' && l[0] != '#' && strings.HasPrefix(l, "payload:") {
			payloadIdx = i
			break
		}
	}
	if payloadIdx < 0 {
		return out
	}
	secEnd := len(lines)
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
	ovIdx, ovIndent := -1, 2
	for i := payloadIdx + 1; i < secEnd; i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		ind := len(l) - len(strings.TrimLeft(l, " \t"))
		if ind == 0 {
			break
		}
		if ovIdx < 0 && reOverrideKey.MatchString(strings.TrimLeft(l, " \t")) {
			ovIdx = i
			ovIndent = ind
		}
	}
	if ovIdx < 0 {
		return out
	}

	var names []string
	val := 0
	flush := func() {
		if val > 0 {
			for _, n := range names {
				out[n] = val
			}
		}
		names = nil
		val = 0
	}
	for i := ovIdx + 1; i < secEnd; i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		ind := len(l) - len(strings.TrimLeft(l, " \t"))
		if ind <= ovIndent {
			break
		}
		trimmed := strings.TrimLeft(l, " \t")
		if reRuleStart.MatchString(trimmed) {
			flush()
		}
		for _, m := range reNameAny.FindAllStringSubmatch(l, -1) {
			if n := strings.Trim(unquote(m[1]), `"`); n != "" {
				names = append(names, n)
			}
		}
		if m := reMaxTokens.FindStringSubmatch(l); m != nil {
			val = num(m[1])
		}
	}
	flush()
	return out
}

// overrideWrites 依据手动设置的输出上限生成 payload.override 追加规则（max_tokens）。
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
			if ovIdx < 0 && reOverrideKey.MatchString(strings.TrimLeft(l, " \t")) {
				ovIdx = i
				ovIndent = ind
			}
		}
	}
	var starts []int
	if ovIdx >= 0 {
		for i := ovIdx + 1; i < secEnd; i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			ind := len(l) - len(strings.TrimLeft(l, " \t"))
			if ind <= ovIndent {
				break
			}
			if ind == ovIndent+2 && reRuleStart.MatchString(strings.TrimLeft(l, " \t")) {
				starts = append(starts, i)
			}
		}
		for ri, s := range starts {
			end := secEnd
			if ri+1 < len(starts) {
				end = starts[ri+1]
			}
			names := map[string]bool{}
			val := 0
			for i := s; i < end; i++ {
				l := lines[i]
				for _, m := range reNameAny.FindAllStringSubmatch(l, -1) {
					if n := strings.Trim(unquote(m[1]), `"`); n != "" {
						names[n] = true
					}
				}
				if m := reMaxTokens.FindStringSubmatch(l); m != nil && val == 0 {
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

// ------------------------- 工具 -------------------------

const indentOfItem = 6

func unquote(s string) string {
	s = strings.TrimSpace(s)
	// 带引号的值后面可能跟行尾注释（如 "x" # 注释）：取首个配对引号内的内容
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end]
		}
	}
	// 裸值：剥离行尾注释
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = s[:idx]
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
