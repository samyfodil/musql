package oidc

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

var urlQueryEscape = url.QueryEscape

// urlValues are query parameters; empty ones are left out.
type urlValues map[string]string

func (v urlValues) encode() string {
	q := url.Values{}
	for k, x := range v {
		if x != "" {
			q.Set(k, x)
		}
	}
	return q.Encode()
}

func render(w http.ResponseWriter, status int, t *template.Template, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The pages take credentials; nothing may frame them.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	t.Execute(w, data)
}

const layout = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{block "title" .}}musql cloud{{end}}</title>
<style>
:root{--bg:#f6f7f9;--card:#fff;--fg:#111827;--muted:#6b7280;--line:#e5e7eb;--accent:#2563eb;--err:#b91c1c}
@media (prefers-color-scheme:dark){:root{--bg:#0b0d12;--card:#151821;--fg:#e5e7eb;--muted:#9ca3af;--line:#262b36;--accent:#60a5fa;--err:#f87171}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
main{width:100%;max-width:360px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:20px;margin:0 0 4px}
p{color:var(--muted);margin:0 0 20px}
label{display:block;font-size:13px;font-weight:600;margin:14px 0 6px}
input{width:100%;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit;font:inherit}
input:focus{outline:2px solid var(--accent);outline-offset:-1px}
.row{display:flex;gap:8px;margin-top:20px}
button{flex:1;padding:10px 12px;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
button.secondary{background:transparent;color:var(--fg);border:1px solid var(--line)}
.error{color:var(--err);font-size:14px;margin:12px 0 0}
.brand{font-weight:700;letter-spacing:-.01em;margin-bottom:18px;color:var(--muted)}
</style></head>
<body><main><div class="brand">musql cloud</div>{{template "body" .}}</main></body></html>`

func page(body string) *template.Template {
	return template.Must(template.Must(template.New("layout").Parse(layout)).Parse(body))
}

var loginTmpl = page(`{{define "title"}}Sign in · musql cloud{{end}}{{define "body"}}
<h1>Sign in</h1><p>Use your email and password.</p>
<form method="post" action="/login">
<input type="hidden" name="next" value="{{.Next}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label for="email">Email</label><input id="email" name="email" type="email" autocomplete="username" value="{{.Email}}" required autofocus>
<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" required>
{{with .Error}}<div class="error">{{.}}</div>{{end}}
<div class="row"><button type="submit">Sign in</button></div>
</form>{{end}}`)

var deviceTmpl = page(`{{define "title"}}Device sign-in · musql cloud{{end}}{{define "body"}}
<h1>Sign in a device</h1><p>Enter the code your device shows, and approve only if you started this sign-in.</p>
<form method="post" action="/oidc/device/verify">
<label for="code">Code</label><input id="code" name="user_code" value="{{.Code}}" placeholder="XXXX-XXXX" autocomplete="off" required autofocus>
{{with .Error}}<div class="error">{{.}}</div>{{end}}
<div class="row"><button class="secondary" name="action" value="deny">Deny</button><button name="action" value="approve">Approve</button></div>
</form>{{end}}`)

var messageTmpl = page(`{{define "title"}}{{.Title}} · musql cloud{{end}}{{define "body"}}<h1>{{.Title}}</h1><p>{{.Message}}</p>{{end}}`)
