package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// AuthSaver persists one credential file through the host's auth-dir. The ABI
// layer injects the host bridge via SetAuthSaver; without it (tests, CLI)
// saving through the host is unavailable.
type AuthSaver func(name string, payload []byte) error

var (
	saverMu    sync.RWMutex
	sharedSaver AuthSaver
)

// SetAuthSaver installs the host auth-dir bridge. Called by the ABI layer on
// plugin registration.
func SetAuthSaver(saver AuthSaver) {
	saverMu.Lock()
	defer saverMu.Unlock()
	sharedSaver = saver
}

// SharedAuthSaver returns the currently installed host bridge, or nil.
func SharedAuthSaver() AuthSaver {
	saverMu.RLock()
	defer saverMu.RUnlock()
	return sharedSaver
}

// WebLogin implements pluginapi.ManagementAPI. It hosts two browser-navigable
// resource routes that make click-through OAuth work when the browser and the
// CPA host are different machines (Zed always redirects the callback to
// 127.0.0.1, which only the desktop client's own machine can serve):
//
//	GET  /v0/resource/plugins/zed/login    -> relay page (opens the Zed sign-in)
//	POST /v0/resource/plugins/zed/complete -> paste the 127.0.0.1 callback URL back
//
// Neither route is management-authenticated, so both are guarded by the
// unguessable login state (144 bits of entropy) and single-use semantics.
type WebLogin struct {
	provider  *Provider
	mu        sync.Mutex
	basePath  string
	resources []pluginapi.ResourceRoute
}

func NewWebLogin(provider *Provider) *WebLogin {
	return &WebLogin{provider: provider}
}

// RegisterManagement declares the resource routes once, on the paths the host
// names as valid for this plugin.
func (w *WebLogin) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.basePath = strings.TrimRight(req.ResourceBasePath, "/")
	w.resources = []pluginapi.ResourceRoute{
		{
			Path:        w.basePath + "/login",
			Description: "Zed browser sign-in relay page.",
			Handler:     w,
		},
		{
			Path:        w.basePath + "/complete",
			Description: "Zed sign-in completion endpoint.",
			Handler:     w,
		},
	}
	return pluginapi.ManagementRegistrationResponse{Resources: w.resources}, nil
}

// HandleManagement serves both resource routes.
func (w *WebLogin) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	w.mu.Lock()
	basePath := w.basePath
	w.mu.Unlock()
	switch {
	case basePath != "" && req.Path == basePath+"/login":
		return w.serveRelayPage(req)
	case basePath != "" && req.Path == basePath+"/complete":
		if !strings.EqualFold(req.Method, http.MethodGet) {
			// Resource routes are dispatched as GET by the host regardless.
			resp, _ := jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
			return resp, nil
		}
		return w.serveComplete(ctx, req)
	}
	resp, _ := jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	return resp, nil
}

func (w *WebLogin) serveRelayPage(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	state := strings.TrimSpace(req.Query.Get("state"))
	session, ok := w.provider.oauth.get(state)
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown or completed login state; start the sign-in again"})
	}
	if auths, result, done := session.outcome(); done {
		return relayStatusPage(state, result, auths)
	}
	page, errRender := renderRelayPage(state, session.signInURL)
	if errRender != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "render login page failed"})
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       page,
	}, nil
}

// serveComplete finishes a login. Resource routes are GET-only, so the relay
// page passes the pasted 127.0.0.1 callback URL as a query parameter (or the
// raw user_id/access_token pair). A request with neither just reports the
// session status, which the page polls to notice an automatic loopback
// completion. The credential is decrypted, validated against Zed, expanded per
// organization, and saved through the host.
func (w *WebLogin) serveComplete(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	state := strings.TrimSpace(req.Query.Get("state"))
	session, ok := w.provider.oauth.get(state)
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown or completed login state; start the sign-in again"})
	}
	if auths, result, done := session.outcome(); done {
		if result != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": result.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"status": "done", "saved": authLabels(auths)})
	}

	captured := callbackResult{
		userID:      strings.TrimSpace(req.Query.Get("user_id")),
		accessToken: strings.TrimSpace(req.Query.Get("access_token")),
	}
	if callbackURL := strings.TrimSpace(req.Query.Get("callback_url")); callbackURL != "" {
		parsed, errParse := url.Parse(callbackURL)
		if errParse != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "callback URL is not a valid URL"})
		}
		values := parsed.Query()
		captured = callbackResult{
			userID:      strings.TrimSpace(values.Get("user_id")),
			accessToken: strings.TrimSpace(values.Get("access_token")),
		}
		if strings.TrimSpace(values.Get("error")) != "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "Zed reported a sign-in error"})
		}
	}
	if captured.userID == "" || captured.accessToken == "" {
		return jsonResponse(http.StatusOK, map[string]string{"status": "pending"})
	}

	auths, errFinalize := expandAndStore(ctx, session, session.settings, captured, "", cliHTTPClient)
	if errFinalize == nil {
		saver := SharedAuthSaver()
		if saver == nil {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "host credential storage is unavailable"})
		}
		for _, auth := range auths {
			if errSave := saver(auth.FileName, auth.StorageJSON); errSave != nil {
				return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "save credential failed: " + errSave.Error()})
			}
		}
	}
	session.recordOutcome(auths, errFinalize)
	w.provider.oauth.remove(state)
	if errFinalize != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": errFinalize.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"status": "ok", "saved": authLabels(auths)})
}

func authLabels(auths []pluginapi.AuthData) []string {
	labels := make([]string, 0, len(auths))
	for _, auth := range auths {
		labels = append(labels, auth.Label)
	}
	return labels
}

func jsonResponse(status int, body any) (pluginapi.ManagementResponse, error) {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError}, errMarshal
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}, nil
}

// relayStatusPage renders the terminal page for an already settled session.
func relayStatusPage(state string, result error, auths []pluginapi.AuthData) (pluginapi.ManagementResponse, error) {
	var message string
	if result != nil {
		message = "登录失败：" + result.Error()
	} else if len(auths) > 0 {
		names := make([]string, 0, len(auths))
		for _, auth := range auths {
			names = append(names, auth.Label)
		}
		message = "登录成功，已保存 " + fmt.Sprint(len(auths)) + " 份凭据：" + strings.Join(names, "、")
	} else {
		message = "登录已结束。"
	}
	page := fmt.Sprintf(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>Zed 登录已结束</title></head><body style="font-family:system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem"><h2>Zed 登录已结束</h2><p>%s</p><p>可以关闭此页面，返回管理面板查看凭据。</p></body></html>`, htmlEscape(message))
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(page),
	}, nil
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return replacer.Replace(value)
}

// renderRelayPage builds the single-page login relay. Everything is inline:
// the page must load on a locked-down host with no external requests.
func renderRelayPage(state, signInURL string) ([]byte, error) {
	data := struct {
		State     string
		SignInURL string
	}{htmlEscape(state), htmlEscape(signInURL)}
	page := fmt.Sprintf(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Zed 账号登录</title>
<style>
body{font:15px/1.6 system-ui,-apple-system,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1.25rem;color:#1a1a1a}
h2{font-size:1.3rem}
.step{margin:1.25rem 0;padding:1rem 1.25rem;border:1px solid #e2e2e2;border-radius:10px}
a.btn,input[type=submit]{display:inline-block;background:#0a72d0;color:#fff;border:none;border-radius:8px;padding:.55rem 1.1rem;font-size:15px;text-decoration:none;cursor:pointer}
input[type=text]{width:100%%;box-sizing:border-box;padding:.5rem;border:1px solid #ccc;border-radius:8px;font-size:14px}
#msg{margin-top:.75rem;word-break:break-all}
.err{color:#b3261e}.ok{color:#116a3c}
small{color:#666}
</style>
</head>
<body>
<h2>Zed 账号登录（浏览器一键 OAuth）</h2>
<div class="step">
<strong>第 1 步</strong>：点击下面的按钮，在新标签页完成 Zed 登录授权。<br><br>
<a class="btn" href="%s" target="_blank" rel="noopener noreferrer">打开 Zed 登录页面</a>
</div>
<div class="step">
<strong>第 2 步</strong>：登录成功后浏览器会跳转到一个 <code>http://127.0.0.1:...</code> 开头、<em>无法打开</em> 的地址（这是正常的）。把地址栏里的完整网址复制粘贴到这里：<br><br>
<form id="f">
<input type="text" id="cb" placeholder="http://127.0.0.1:12345/?user_id=...&amp;access_token=..." autocomplete="off">
<input type="submit" value="完成登录">
</form>
<div id="msg"></div>
</div>
<p><small>此页面由你的 CLIProxyAPI 实例直接提供，凭据只会提交到本实例并保存在其 auth 目录中。本页面约 %s 后过期，请从管理面板重新发起。</small></p>
<script>
var f=document.getElementById('f');
f.addEventListener('submit',function(ev){ev.preventDefault();
var v=document.getElementById('cb').value.trim();
var m=document.getElementById('msg');
if(!v){m.textContent='请先粘贴回调地址';m.className='err';return}
m.textContent='正在验证...';m.className='';
fetch('/v0/resource/plugins/zed/complete?state=%s&callback_url='+encodeURIComponent(v))
.then(function(r){return r.json().then(function(j){return {s:r.status,j:j}})})
.then(function(o){
 if(o.s===200&&(o.j.status==='ok'||o.j.status==='done')){m.textContent='登录成功：'+((o.j.saved||[]).join('、')||'凭据已保存')+'。可以关闭此页面，返回管理面板。';m.className='ok'}
 else{m.textContent='失败：'+(o.j&&o.j.error||('HTTP '+o.s));m.className='err'}})
.catch(function(e){m.textContent='请求失败：'+e;m.className='err'})});
var timer=setInterval(function(){
fetch('/v0/resource/plugins/zed/complete?state=%s')
.then(function(r){return r.json().then(function(j){return {s:r.status,j:j}})})
.then(function(o){
 if(o.s===200&&(o.j.status==='done'||o.j.status==='ok')){m.textContent='登录已完成（本机回调自动捕获）。可以关闭此页面。';m.className='ok';clearInterval(timer)}})
.catch(function(){})},2000);
</script>
</body>
</html>
`, data.SignInURL, htmlEscape(loginTimeout.String()), data.State, data.State)
	return []byte(page), nil
}
