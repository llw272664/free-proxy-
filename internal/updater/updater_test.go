package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		tag  string
		want version
		ok   bool
	}{
		{"v0.1.21", version{0, 1, 21}, true},
		{"0.1.21", version{0, 1, 21}, true},
		{"v1.0", version{1, 0, 0}, true},
		// `git describe` on a commit past a tag: the suffix counts commits, not
		// version, so the build compares as the tag it followed.
		{"v0.1.21-4-gdeadbee", version{0, 1, 21}, true},
		{"dev", version{}, false},
		{"deadbee", version{}, false},
		{"", version{}, false},
		{"v1.2.3.4", version{}, false},
	}
	for _, c := range cases {
		got, ok := parseVersion(c.tag)
		if ok != c.ok || got != c.want {
			t.Errorf("parseVersion(%q) = %v,%v; want %v,%v", c.tag, got, ok, c.want, c.ok)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.1.22", "v0.1.21", true},
		{"v0.2.0", "v0.1.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.21", "v0.1.21", false},
		{"v0.1.21", "v0.1.22", false},
		// A development build off v0.1.21 must not be offered v0.1.21 again.
		{"v0.1.21", "v0.1.21-4-gdeadbee", false},
	}
	for _, c := range cases {
		a, _ := parseVersion(c.a)
		b, _ := parseVersion(c.b)
		if got := a.newer(b); got != c.want {
			t.Errorf("%s newer than %s = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseNotesReadsSections(t *testing.T) {
	body := strings.Join([]string{
		"## 新功能",
		"- 后台一键更新",
		"- feat: 节点搜索支持中文",
		"",
		"## 问题修复",
		"* 仪表盘只统计可用节点",
		"",
		"**Full Changelog**: https://github.com/o/r/compare/v0.1.20...v0.1.21",
	}, "\n")

	got := parseNotes(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 sections, got %+v", got)
	}
	if got[0].Title != "新功能" || len(got[0].Items) != 2 {
		t.Errorf("first section is %+v", got[0])
	}
	// A conventional-commit prefix inside a section the heading already names
	// is noise, and is stripped.
	if got[0].Items[1] != "节点搜索支持中文" {
		t.Errorf("prefix survived: %q", got[0].Items[1])
	}
	if got[1].Title != "问题修复" || got[1].Items[0] != "仪表盘只统计可用节点" {
		t.Errorf("second section is %+v", got[1])
	}
}

// GitHub's own generated notes are a bare compare link for a repository that
// merges no pull requests. An empty result is what tells the caller to rebuild
// the changelog from the commit range instead.
func TestParseNotesIgnoresGeneratedBody(t *testing.T) {
	if got := parseNotes("**Full Changelog**: https://github.com/o/r/compare/v0.1.20...v0.1.21"); got != nil {
		t.Errorf("expected no sections, got %+v", got)
	}
	if got := parseNotes(""); got != nil {
		t.Errorf("expected no sections for an empty body, got %+v", got)
	}
}

func TestGroupCommits(t *testing.T) {
	got := groupCommits([]string{
		"feat: search nodes in Chinese",
		"fix(api): count only ready nodes",
		"chore: refresh typescript build cache",
		"perf: bound what maintenance keeps growing",
		"Update README.md",
		"feat!: drop the legacy settings form",
	})

	want := []Section{
		{Title: sectionFeature, Items: []string{"search nodes in Chinese", "drop the legacy settings form"}},
		{Title: sectionFix, Items: []string{"count only ready nodes"}},
		{Title: sectionPerf, Items: []string{"bound what maintenance keeps growing"}},
		{Title: sectionOther, Items: []string{"refresh typescript build cache", "Update README.md"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("grouped as\n%v\nwant\n%v", got, want)
	}
}

// releaseJSON renders the parts of the GitHub payload this package reads.
func releaseJSON(tag, body string, assets ...string) map[string]any {
	var list []map[string]any
	for _, name := range assets {
		list = append(list, map[string]any{
			"name": name, "browser_download_url": "https://github.com/o/r/releases/download/" + tag + "/" + name,
		})
	}
	return map[string]any{
		"tag_name": tag, "body": body, "draft": false, "prerelease": false,
		"published_at": "2026-09-02T04:32:08Z",
		"html_url":     "https://github.com/o/r/releases/tag/" + tag,
		"assets":       list,
	}
}

func testService(t *testing.T, handler http.HandlerFunc, current string) *Service {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := New("o/r", current, "")
	s.baseURL = srv.URL
	s.client = srv.Client()
	return s
}

func TestStatusListsEveryVersionInBetween(t *testing.T) {
	binary := assetPrefix + "amd64"
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/releases") {
			t.Errorf("unexpected request %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.22", "## 新功能\n- 后台一键更新", binary, checksumAsset),
			releaseJSON("v0.1.21", "## 问题修复\n- 修复节点统计", binary, checksumAsset),
			releaseJSON("v0.1.20", "## 文档\n- 更新说明", binary, checksumAsset),
			// A release with no checksum file cannot be verified, so it is not
			// a release this updater will install.
			releaseJSON("v0.1.23", "## 新功能\n- 不该出现", binary),
		})
	}, "v0.1.20")

	st, err := s.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.LatestVersion != "v0.1.22" || !st.Available {
		t.Fatalf("latest=%q available=%v", st.LatestVersion, st.Available)
	}
	if len(st.Pending) != 2 {
		t.Fatalf("expected v0.1.22 and v0.1.21 pending, got %+v", st.Pending)
	}
	if st.Pending[0].Version != "v0.1.22" || st.Pending[1].Version != "v0.1.21" {
		t.Errorf("pending order is %s, %s", st.Pending[0].Version, st.Pending[1].Version)
	}
	if st.Pending[0].Sections[0].Items[0] != "后台一键更新" {
		t.Errorf("notes not carried through: %+v", st.Pending[0].Sections)
	}
}

func TestStatusUpToDate(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.21", "", assetPrefix+"amd64", checksumAsset),
		})
	}, "v0.1.21")

	st, err := s.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Available || len(st.Pending) != 0 {
		t.Errorf("an up-to-date install was offered %+v", st.Pending)
	}
}

// A build whose version cannot be ordered ("dev") is offered the newest release
// and nothing else: claiming to know which releases it is missing would be a
// guess dressed as a changelog.
func TestStatusUnknownVersionOffersLatestOnly(t *testing.T) {
	binary := assetPrefix + "amd64"
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.22", "## 新功能\n- 一", binary, checksumAsset),
			releaseJSON("v0.1.21", "## 新功能\n- 二", binary, checksumAsset),
		})
	}, "dev")

	st, err := s.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Available || len(st.Pending) != 1 || st.Pending[0].Version != "v0.1.22" {
		t.Errorf("dev build was offered %+v", st.Pending)
	}
}

// A release whose body says nothing gets its changelog rebuilt from the commits
// between it and the version installed here.
func TestStatusRebuildsNotesFromCommits(t *testing.T) {
	binary := assetPrefix + "amd64"
	var comparePath string
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/compare/") {
			comparePath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"commits": []map[string]any{
				{"commit": map[string]any{"message": "fix: keep probing when discovery fails\n\nbody"}},
				{"commit": map[string]any{"message": "feat: 后台一键更新"}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.22", "**Full Changelog**: https://github.com/o/r/compare/v0.1.21...v0.1.22", binary, checksumAsset),
		})
	}, "v0.1.21")

	st, err := s.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.HasSuffix(comparePath, "/compare/v0.1.21...v0.1.22") {
		t.Errorf("compared %q", comparePath)
	}
	if len(st.Pending) != 1 || len(st.Pending[0].Sections) != 2 {
		t.Fatalf("rebuilt notes are %+v", st.Pending)
	}
	// Newest commit first, and only the subject line of each.
	if got := st.Pending[0].Sections[0]; got.Title != sectionFeature || got.Items[0] != "后台一键更新" {
		t.Errorf("first section is %+v", got)
	}
	if got := st.Pending[0].Sections[1]; got.Title != sectionFix || got.Items[0] != "keep probing when discovery fails" {
		t.Errorf("second section is %+v", got)
	}
}

// The console asks on every visit to the system tab; GitHub allows sixty
// unauthenticated calls an hour. Only a forced check may leave this host.
func TestStatusCachesUntilForced(t *testing.T) {
	calls := 0
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.21", "", assetPrefix+"amd64", checksumAsset),
		})
	}, "v0.1.21")

	ctx := context.Background()
	for range 3 {
		if _, err := s.Status(ctx, false); err != nil {
			t.Fatalf("status: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("three checks made %d requests", calls)
	}
	if _, err := s.Status(ctx, true); err != nil {
		t.Fatalf("forced status: %v", err)
	}
	if calls != 2 {
		t.Errorf("a forced check made %d requests in total", calls)
	}

	// An expired cache goes back to GitHub on its own.
	s.cachedAt = s.cachedAt.Add(-2 * checkTTL)
	if _, err := s.Status(ctx, false); err != nil {
		t.Fatalf("status: %v", err)
	}
	if calls != 3 {
		t.Errorf("an expired check made %d requests in total", calls)
	}
}

// A GitHub outage must not erase what the last successful check found.
func TestStatusKeepsLastResultWhenGitHubFails(t *testing.T) {
	fail := false
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.22", "## 新功能\n- 一", assetPrefix+"amd64", checksumAsset),
		})
	}, "v0.1.21")

	ctx := context.Background()
	if _, err := s.Status(ctx, true); err != nil {
		t.Fatalf("status: %v", err)
	}
	fail = true
	st, err := s.Status(ctx, true)
	if err != nil {
		t.Fatalf("a failed re-check should keep the previous answer: %v", err)
	}
	if st.LatestVersion != "v0.1.22" {
		t.Errorf("latest version was forgotten: %+v", st)
	}
}

func TestStatusReportsFirstFailure(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}, "v0.1.21")
	_, err := s.Status(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Errorf("rate limit reported as %v", err)
	}
}

func TestExpectedSum(t *testing.T) {
	digest := strings.Repeat("a", 64)
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  free-proxy-linux-arm64\n%s  free-proxy-linux-amd64\n", strings.Repeat("b", 64), digest)
	}, "v0.1.21")
	// fetch() only accepts github.com, so point the asset at the test server
	// through the same code path the release listing would have filled in.
	rel := ghRelease{TagName: "v0.1.22", Assets: []ghAsset{{Name: checksumAsset, URL: "https://github.com/o/r/SHA256SUMS"}}}
	s.client = &http.Client{Transport: rewriteHost{t: t, target: s.baseURL}}

	got, err := s.expectedSum(context.Background(), rel, "free-proxy-linux-amd64")
	if err != nil {
		t.Fatalf("expected sum: %v", err)
	}
	if got != digest {
		t.Errorf("digest = %q", got)
	}
	if _, err := s.expectedSum(context.Background(), rel, "free-proxy-linux-riscv64"); err == nil {
		t.Error("a missing asset should not resolve to a digest")
	}
}

// rewriteHost sends a github.com request to the test server, so the host check
// in fetch stays exercised.
type rewriteHost struct {
	t      *testing.T
	target string
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "github.com" {
		r.t.Errorf("download left GitHub: %s", req.URL)
	}
	clone := req.Clone(req.Context())
	target, err := http.NewRequest(req.Method, r.target+req.URL.Path, nil)
	if err != nil {
		return nil, err
	}
	clone.URL = target.URL
	clone.Host = target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestFetchRefusesNonGitHubURL(t *testing.T) {
	s := New("o/r", "v0.1.21", "")
	if _, err := s.fetch(context.Background(), "https://example.com/free-proxy-linux-amd64"); err == nil {
		t.Error("a download from outside GitHub was allowed")
	}
}

func TestTailLogReturnsTheEnd(t *testing.T) {
	path := t.TempDir() + "/update.log"
	var b strings.Builder
	for i := range logTailLines + 20 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New("o/r", "v0.1.21", path)
	lines := strings.Split(s.tailLog(), "\n")
	if len(lines) != logTailLines {
		t.Fatalf("kept %d lines", len(lines))
	}
	if lines[len(lines)-1] != fmt.Sprintf("line %d", logTailLines+19) {
		t.Errorf("last line is %q", lines[len(lines)-1])
	}
	if s := New("o/r", "v0.1.21", "/nonexistent/update.log"); s.tailLog() != "" {
		t.Error("a missing log should read as empty")
	}
}

func TestPublishedAtSurvivesTheRoundTrip(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			releaseJSON("v0.1.22", "## 新功能\n- 一", assetPrefix+"amd64", checksumAsset),
		})
	}, "v0.1.21")
	st, err := s.Status(context.Background(), true)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	want := time.Date(2026, 9, 2, 4, 32, 8, 0, time.UTC)
	if !st.Pending[0].PublishedAt.Equal(want) {
		t.Errorf("published at %v", st.Pending[0].PublishedAt)
	}
}
