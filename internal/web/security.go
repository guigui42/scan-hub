package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/guigui42/scan-hub/internal/fsutil"
)

const (
	csrfCookie = "scanhub_csrf"
	csrfHeader = "X-CSRF-Token"
	maxForm    = 64 << 10
)

// csp allows only same-origin scripts and styles: no inline code and no
// eval (Alpine runs as its CSP build; htmx attributes avoid expressions).
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// loadKey reads or creates the CSRF HMAC key, so tokens in open pages stay
// valid across restarts.
func loadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k := make([]byte, 32)
	rand.Read(k)
	if err := fsutil.WriteFileAtomic(path, k, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func (s *Server) csrfToken(cookie string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(cookie))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ensureCSRF returns the token for this browser, setting the cookie if needed.
func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
		return s.csrfToken(c.Value)
	}
	b := make([]byte, 32)
	rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: v, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r), MaxAge: 400 * 24 * 3600,
	})
	return s.csrfToken(v)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// secure wraps the app with host, origin, CSRF, body-size and header policy.
func (s *Server) secure(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")

		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hostAllowed(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
			return
		case http.MethodPost, http.MethodDelete:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := cop.Check(r); err != nil {
			s.fail(w, r, http.StatusForbidden, "Cross-origin request refused.")
			return
		}
		if o := r.Header.Get("Origin"); o == "" || !sameOrigin(o, r.Host) {
			s.fail(w, r, http.StatusForbidden, "Request refused: missing or foreign Origin.")
			return
		}
		c, err := r.Cookie(csrfCookie)
		if err != nil || !hmac.Equal([]byte(r.Header.Get(csrfHeader)), []byte(s.csrfToken(c.Value))) {
			s.fail(w, r, http.StatusForbidden, "This page has expired. Reload it and try again.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(host string) bool {
	return slices.ContainsFunc(s.cfg.AllowedHosts, func(a string) bool { return strings.EqualFold(a, host) })
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	// Origin omits default ports that Host may carry.
	h, p, err := net.SplitHostPort(host)
	return err == nil && strings.EqualFold(u.Host, h) && ((u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80"))
}
