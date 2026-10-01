package api

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func adminLoginPage(w http.ResponseWriter, r *http.Request) {
	if managers.Settings.IsSetupDone() {
		if c, err := r.Cookie(adminSessionCookie); err == nil && isValidAdminSession(c.Value) {
			http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
			return
		}
	} else {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, loginHTML(false))
}

// isSecureRequest reports whether the browser reached mycelium over HTTPS,
// for the purpose of setting the admin session cookie's Secure attribute.
//
// r.TLS != nil alone is only true for a direct TLS connection to this
// process. A TLS-terminating reverse proxy (the common deployment, already
// handled elsewhere via X-Forwarded-Proto — see media_handler.go's
// proxyScheme resolution and grpcweb.go's X-Http-Scheme synthesis) always
// talks plain HTTP to mycelium, so r.TLS is nil even though the real client
// spoke HTTPS: without this, the admin cookie would never get Secure behind
// such a proxy.
//
// X-Forwarded-Proto can't be trusted unconditionally though — a client that
// isn't behind any proxy can set it on a direct plain-HTTP request too. So
// it's honoured only when the direct TCP peer is a trusted proxy, reusing
// the same trustedProxyNets/isTrustedProxy notion ratelimit.go's realIP()
// already applies to X-Forwarded-For (loopback by default, or the operator's
// MYCELIUM_TRUSTED_PROXY_CIDRS).
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if isTrustedProxy(directRemoteHost(r)) && r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	return false
}

// setAdminSessionCookie creates a new admin session and attaches it to the
// response — shared by the login form and saveSetup (which auto-logs-in the
// browser that just created the admin account, so it can immediately call
// other /admin/* endpoints straight after the setup wizard).
func setAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	token := createAdminSession()
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   43200, // 12h
	})
}

func adminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminSessionCookie); err == nil && isValidAdminSession(c.Value) {
		http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	password := r.FormValue("password")
	if err := core.VerifyAdmin(password, managers.Settings.MasterAdminHash()); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, loginHTML(true))
		return
	}
	setAdminSessionCookie(w, r)
	http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
}

func adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminSessionCookie); err == nil {
		revokeAdminSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   adminSessionCookie,
		Value:  "",
		Path:   "/admin",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// loginHTML renders the admin login page — mobile-first, matching the
// dashboard/setup visual language (dark, purple hexagon mark, safe-area).
// withErr adds the "wrong password" line.
func loginHTML(withErr bool) string {
	errLine := ""
	if withErr {
		errLine = `<p style="color:#f87171;font-size:.8rem;margin:0 0 .75rem">Password errata.</p>`
	}
	return `<!DOCTYPE html><html lang="it"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="theme-color" content="#0a0d12">
<title>Mycelium — Accesso</title>
<link rel="icon" href="/static/icon-192.png">
<style>
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       padding:calc(env(safe-area-inset-top) + 1.5rem) 1.25rem calc(env(safe-area-inset-bottom) + 1.5rem);
       background:#0a0d12;color:#e5e7eb;
       font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;
       -webkit-tap-highlight-color:transparent}
  .box{width:100%;max-width:22rem;background:#141820;border:1px solid #1e2530;
       border-radius:1rem;padding:1.75rem}
  .mark{width:3rem;height:3rem;border-radius:.85rem;display:flex;align-items:center;justify-content:center;
        margin:0 auto .9rem;background:linear-gradient(135deg,#7c6af7,#5a4fd4)}
  h1{font-size:1.15rem;font-weight:700;color:#fff;text-align:center;margin:0 0 .25rem}
  p.sub{font-size:.8rem;color:#6b7280;text-align:center;margin:0 0 1.4rem}
  label{display:block;font-size:.75rem;color:#9ca3af;margin-bottom:.4rem}
  input{width:100%;padding:.8rem .9rem;font-size:1rem;border-radius:.6rem;
        border:1px solid #2a3040;background:#0d1017;color:#fff;outline:none}
  input:focus{border-color:#7c6af7;box-shadow:0 0 0 3px rgba(124,106,247,.15)}
  button{width:100%;margin-top:1rem;padding:.85rem;font-size:1rem;font-weight:700;
         border:0;border-radius:.6rem;background:#7c6af7;color:#fff;cursor:pointer}
  button:active{background:#6a58e8}
</style></head>
<body><div class="box">
  <div class="mark"><svg viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2"
       stroke-linecap="round" stroke-linejoin="round" width="26" height="26">
       <path d="M12 2L2 7l10 5 10-5-10-5z"/><path d="M2 17l10 5 10-5"/><path d="M2 12l10 5 10-5"/></svg></div>
  <h1>Mycelium</h1>
  <p class="sub">Accesso amministratore</p>
  <form method="POST" action="/admin/login">
    ` + errLine + `
    <label for="pw">Password</label>
    <input id="pw" type="password" name="password" autocomplete="current-password"
           autofocus required>
    <button type="submit">Accedi</button>
  </form>
</div>
</body></html>`
}

func adminDashboard(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles(filepath.Join(core.BasePath, "web", "templates", "admin.html"))
	if err != nil {
		http.Error(w, "Template non trovato", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("[admin] template execute: %v", err)
	}
}
