package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/masteralanlab/free-proxy/internal/security"
)

// ExternalAccessGuard blocks non-loopback clients from the web admin when the
// admin toggle disables external web access. It returns 404 (like SecretPath) to
// avoid revealing the service. Registered before SecretPath. Uses the real TCP
// remote address (not X-Forwarded-For) so it cannot be spoofed by a header.
func ExternalAccessGuard(store *security.AdminConfigStore) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if store.Config().WebExternalAllowed() || isLoopbackRemote(c.Request().RemoteAddr) {
				return next(c)
			}
			return echo.NewHTTPError(http.StatusNotFound, "Not found")
		}
	}
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const (
	ctxAuthorized = "authorized"
	ctxSession    = "session_token"
)

// SecretPath is a pre-router middleware: requests must start with the secret
// path prefix (else 404, hiding the service), the prefix is stripped, and
// non-public API routes require a valid session (else 401).
func SecretPath(auth *security.AuthService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			prefix := "/" + auth.Store.Config().SecretPath
			req := c.Request()
			p := req.URL.Path
			if p == prefix {
				return c.Redirect(http.StatusTemporaryRedirect, prefix+"/")
			}
			if !strings.HasPrefix(p, prefix+"/") {
				return echo.NewHTTPError(http.StatusNotFound, "Not found")
			}
			stripped := strings.TrimPrefix(p, prefix)
			req.URL.Path = stripped
			// The router matches on RawPath whenever it is set — which is
			// whenever the request path contains a percent-encoded character.
			// Leaving it unstripped routes those requests against the prefixed
			// path, which matches no API route and lands them on the SPA
			// fallback: a node id needing encoding would get index.html back
			// with a 200 instead of its data. The prefix is alphanumeric, so
			// its raw and decoded forms are identical.
			if req.URL.RawPath != "" {
				req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, prefix)
			}

			token := validSession(c, auth.Sessions)
			authed := token != ""
			c.Set(ctxAuthorized, authed)
			c.Set(ctxSession, token)

			if !authed && !isPublic(stripped) {
				return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
			}
			return next(c)
		}
	}
}

// isPublic reports whether a stripped path bypasses session auth. All non-API
// paths (SPA + static) are public; only /api/* is protected, except login.
func isPublic(path string) bool {
	if path == "/api/v1/auth/login" {
		return true
	}
	return !strings.HasPrefix(path, "/api/")
}

// validSession returns the first session cookie on the request that names a
// live session, or "" when none does.
//
// A browser can hold more than one cookie called "session" that this request
// carries. Cookies are not port-scoped, so any other panel on the same host
// shares the name with us, and a console served from "/" leaves one that
// matches every path we ever move to. Reading only the first — which is what
// Request.Cookie returns — means one stale value locks the console out for as
// long as that cookie lives: the login succeeds and sets a good cookie, and
// every request after it still answers 401. Checking them all costs a map
// lookup each.
func validSession(c *echo.Context, sessions *security.SessionManager) string {
	for _, cookie := range c.Request().Cookies() {
		if cookie.Name == "session" && sessions.Valid(cookie.Value) {
			return cookie.Value
		}
	}
	return ""
}
