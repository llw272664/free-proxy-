package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masteralanlab/free-proxy/internal/config"
	"github.com/masteralanlab/free-proxy/internal/security"
	"github.com/masteralanlab/free-proxy/internal/store"
)

type console struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	repos  *store.Repos
	cfg    *config.Config
}

func newConsole(t *testing.T) *console {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open("file:" + filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	c := &console{t: t, repos: store.NewRepos(db), cfg: &config.Config{
		DataDir: dir, AdminUsername: "admin", AdminPassword: "Passw0rd1",
		AdminSecretPath: "oldpath", AllowProcessRestart: false,
	}}
	jar, _ := cookiejar.New(nil)
	c.client = &http.Client{Jar: jar}
	c.restart()
	return c
}

// restart replaces the running server with a new one over the same database:
// a fresh admin config read and, like the real process, an empty session map.
func (c *console) restart() {
	c.t.Helper()
	if c.srv != nil {
		c.srv.Close()
	}
	adminStore, err := security.NewAdminConfigStore(c.cfg, c.repos.App)
	if err != nil {
		c.t.Fatal(err)
	}
	auth := security.NewAuthService(c.cfg, adminStore, security.NewSessionManager(config.SessionTTL))
	c.srv = httptest.NewServer(NewServer(&Deps{Cfg: c.cfg, Repos: c.repos, Auth: auth}))
	c.t.Cleanup(c.srv.Close)
}

func (c *console) do(method, path string, body any, header ...string) (int, string) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(data)
}

func (c *console) login(prefix string) {
	c.t.Helper()
	if code, body := c.do("POST", prefix+"/api/v1/auth/login",
		map[string]string{"username": "admin", "password": "Passw0rd1"}); code != 200 {
		c.t.Fatalf("login at %s: %d %s", prefix, code, body)
	}
}

// Changing the management path from the console has to leave an operator able
// to log in at the new one after the service restarts.
func TestChangeSecretPathThenLogin(t *testing.T) {
	c := newConsole(t)
	c.login("/oldpath")

	code, body := c.do("GET", "/oldpath/api/v1/system/config", nil)
	if code != 200 {
		t.Fatalf("system/config: %d %s", code, body)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(body), &settings); err != nil {
		t.Fatal(err)
	}
	settings["admin"].(map[string]any)["secret_path"] = "newpath"
	if code, body = c.do("PUT", "/oldpath/api/v1/system/config",
		map[string]any{"settings": settings, "admin_password": "", "proxy_password": ""}); code != 200 {
		t.Fatalf("save new path: %d %s", code, body)
	}

	c.restart()

	if code, body := c.do("GET", "/oldpath/", nil); code != 404 {
		t.Errorf("old path still served: %d %s", code, body)
	}
	if code, body := c.do("GET", "/newpath/", nil); code != 200 {
		t.Errorf("SPA at the new path: %d %s", code, body)
	}
	c.login("/newpath")
	if code, body := c.do("GET", "/newpath/api/v1/auth/config", nil); code != 200 {
		t.Fatalf("authenticated call after logging in at the new path: %d %s", code, body)
	}
}

// A browser can present more than one cookie named "session" — one left by a
// console served from "/", or one from another panel on the same host, since
// cookies are not port-scoped. Reading only the first locks the console out:
// the login succeeds and every request after it still answers 401.
func TestStaleSessionCookieDoesNotShadowTheLiveOne(t *testing.T) {
	c := newConsole(t)
	c.login("/oldpath")

	var live string
	for _, cookie := range c.client.Jar.Cookies(mustParse(t, c.srv.URL+"/oldpath/")) {
		if cookie.Name == "session" {
			live = cookie.Value
		}
	}
	if live == "" {
		t.Fatal("no session cookie was issued")
	}

	for _, header := range []string{
		"session=stale; session=" + live,
		"session=" + live + "; session=stale",
	} {
		bare := &http.Client{}
		req, _ := http.NewRequest("GET", c.srv.URL+"/oldpath/api/v1/auth/config", nil)
		req.Header.Set("Cookie", header)
		res, err := bare.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("Cookie: %q -> %d, want 200", header, res.StatusCode)
		}
	}
}

// The router matches on URL.RawPath whenever it is set, so stripping the secret
// prefix from URL.Path alone sent every percent-encoded request to the SPA
// fallback: an API call answered 200 with index.html instead of its data.
func TestEncodedPathReachesItsAPIRoute(t *testing.T) {
	c := newConsole(t)
	c.login("/oldpath")

	code, body := c.do("GET", "/oldpath/api/v1/proxies/a%2Fb/probes", nil)
	if code != 200 {
		t.Fatalf("encoded node id: %d %s", code, body)
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("encoded path fell through to the SPA: %s", body)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
