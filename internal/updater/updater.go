package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/masteralanlab/free-proxy/internal/platform"
)

// DefaultRepo is where releases are published. It is overridable through
// FREE_PROXY_REPO for forks, which is the same variable install.sh reads.
const DefaultRepo = "MasterAlanLab/free-proxy"

const (
	apiBase = "https://api.github.com"

	// assetPrefix and checksumAsset are the names release.yml publishes. The
	// checksum file is required, not optional: it is the only thing that ties
	// the bytes we run to the release we read.
	assetPrefix   = "free-proxy-linux-"
	checksumAsset = "SHA256SUMS"

	// checkTTL is how long one release check is reused. GitHub allows 60
	// unauthenticated calls an hour per address, and the console asks on every
	// visit to the system tab; a check that costs nothing until it is ten
	// minutes old keeps that budget for the update itself.
	checkTTL = 10 * time.Minute

	// releasePageSize bounds the release list; notesLookupLimit bounds how many
	// of the pending ones get a reconstructed changelog, since each costs one
	// more API call.
	releasePageSize  = 20
	notesLookupLimit = 5

	apiTimeout      = 20 * time.Second
	downloadTimeout = 15 * time.Minute
	maxJSONBytes    = 8 << 20

	// logTailLines is how much of the last update's output the console shows.
	logTailLines = 80
	logTailBytes = 64 << 10
)

// Status is everything the console needs to decide whether to offer an update.
type Status struct {
	CurrentVersion string    `json:"current_version"`
	LatestVersion  string    `json:"latest_version"`
	Available      bool      `json:"update_available"`
	Pending        []Release `json:"pending"`
	Supported      bool      `json:"supported"`
	Reason         string    `json:"unsupported_reason"`
	Updating       bool      `json:"updating"`
	CheckedAt      time.Time `json:"checked_at"`
	ReleasesURL    string    `json:"releases_url"`
	LastLog        string    `json:"last_log"`
}

// Service checks for and installs new releases.
type Service struct {
	repo    string
	current string
	logPath string
	client  *http.Client
	baseURL string
	now     func() time.Time

	// mu guards the cached check. It is held across the GitHub calls so two
	// console tabs asking at once make one request, not two.
	mu         sync.Mutex
	cached     *Status
	cachedAt   time.Time
	target     *ghRelease
	installing atomic.Bool
}

// New builds a Service for the given repository ("owner/name"), the version
// this binary was stamped with, and the file the update's own output is
// appended to.
func New(repo, current, logPath string) *Service {
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	return &Service{
		repo: strings.Trim(strings.TrimSpace(repo), "/"), current: current, logPath: logPath,
		// No client timeout: a release check and a 16 MB download want very
		// different deadlines, so each request carries its own.
		client:  &http.Client{},
		baseURL: apiBase,
		now:     time.Now,
	}
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []ghAsset `json:"assets"`

	version version
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Status reports the installed version against the published ones. The result
// is cached for checkTTL; force skips the cache, which is what the console's
// "检查更新" button does.
func (s *Service) Status(ctx context.Context, force bool) (*Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && s.cached != nil && s.now().Sub(s.cachedAt) < checkTTL {
		return s.decorate(*s.cached), nil
	}
	releases, err := s.fetchReleases(ctx)
	if err != nil {
		// A check that fails is not a reason to forget what the last one said:
		// the console keeps showing the known state and the operator can retry.
		if s.cached != nil {
			return s.decorate(*s.cached), nil
		}
		return nil, err
	}
	st := s.compare(ctx, releases)
	s.cached, s.cachedAt = st, s.now()
	return s.decorate(*st), nil
}

// decorate fills in the parts of a status that are cheap and always current:
// whether an install is running now, whether this host can self-update at all,
// and the tail of the last update's log.
func (s *Service) decorate(st Status) *Status {
	st.Updating = s.installing.Load()
	if err := SelfUpdateSupport(); err != nil {
		st.Supported, st.Reason = false, err.Error()
	} else {
		st.Supported, st.Reason = true, ""
	}
	st.LastLog = s.tailLog()
	return &st
}

// compare turns the published releases into a status and remembers which one an
// update would install.
func (s *Service) compare(ctx context.Context, releases []ghRelease) *Status {
	st := &Status{
		CurrentVersion: s.current,
		CheckedAt:      s.now().UTC(),
		ReleasesURL:    "https://github.com/" + s.repo + "/releases",
	}
	s.target = nil
	if len(releases) == 0 {
		return st
	}
	latest := releases[0]
	st.LatestVersion = latest.TagName

	current, known := parseVersion(s.current)
	var pending []ghRelease
	for _, r := range releases {
		if known && !r.version.newer(current) {
			break // releases are sorted newest first
		}
		pending = append(pending, r)
		if !known {
			// A build with no comparable version ("dev", or a commit hash) can
			// only be offered the newest release; listing every published one
			// as pending would claim to know what it is missing.
			break
		}
	}
	if len(pending) == 0 {
		return st
	}
	s.target = &latest
	st.Available = true
	st.Pending = s.describe(ctx, pending, releases)
	return st
}

// describe attaches a changelog to each pending release, reconstructing it from
// the commit range when the release body carries no list of its own.
func (s *Service) describe(ctx context.Context, pending, all []ghRelease) []Release {
	out := make([]Release, 0, len(pending))
	lookups := 0
	for _, r := range pending {
		rel := Release{Version: r.TagName, PublishedAt: r.PublishedAt, URL: r.HTMLURL}
		rel.Sections = parseNotes(r.Body)
		if len(rel.Sections) == 0 && lookups < notesLookupLimit {
			if base := previousTag(r, all, s.current); base != "" {
				lookups++
				rel.Sections = s.notesFromCommits(ctx, base, r.TagName)
			}
		}
		out = append(out, rel)
	}
	return out
}

// previousTag is what a release is compared against: the release published just
// below it, or — for the oldest pending one — the version installed here.
func previousTag(r ghRelease, all []ghRelease, current string) string {
	for i, candidate := range all {
		if candidate.TagName != r.TagName {
			continue
		}
		if i+1 < len(all) {
			return all[i+1].TagName
		}
		break
	}
	if _, ok := parseVersion(current); ok {
		return current
	}
	return ""
}

// fetchReleases lists published releases newest first, keeping only the ones
// this installer could actually use.
func (s *Service) fetchReleases(ctx context.Context) ([]ghRelease, error) {
	var raw []ghRelease
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", s.baseURL, s.repo, releasePageSize)
	if err := s.getJSON(ctx, url, &raw); err != nil {
		return nil, err
	}
	out := make([]ghRelease, 0, len(raw))
	for _, r := range raw {
		v, ok := parseVersion(r.TagName)
		if !ok || r.Draft || r.Prerelease || findAsset(r, checksumAsset) == nil {
			continue
		}
		r.version = v
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].version.newer(out[j].version) })
	return out, nil
}

// notesFromCommits rebuilds a changelog from the commits between two tags. It
// is the fallback for releases whose notes were generated before the workflow
// wrote them, and it fails quietly: a missing changelog is worth less than a
// failed update check.
func (s *Service) notesFromCommits(ctx context.Context, base, head string) []Section {
	var payload struct {
		Commits []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	url := fmt.Sprintf("%s/repos/%s/compare/%s...%s", s.baseURL, s.repo, base, head)
	if err := s.getJSON(ctx, url, &payload); err != nil {
		return nil
	}
	subjects := make([]string, 0, len(payload.Commits))
	for i := len(payload.Commits) - 1; i >= 0; i-- { // newest first
		subjects = append(subjects, firstLine(payload.Commits[i].Commit.Message))
	}
	return groupCommits(subjects)
}

func (s *Service) getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "free-proxy-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("连接 GitHub 失败：%w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests:
		return errors.New("GitHub 接口调用次数已达上限，请稍后再试")
	case http.StatusNotFound:
		return fmt.Errorf("找不到仓库 %s 的发布信息", s.repo)
	default:
		return fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(out)
}

// Install downloads the newest release, verifies it, replaces this binary and
// hands the rest of the upgrade to the new one. It is the body of the console's
// update job; the process it runs in is stopped a few seconds after it returns.
func (s *Service) Install(ctx context.Context) (map[string]any, error) {
	if !s.installing.CompareAndSwap(false, true) {
		return nil, errors.New("更新正在进行中")
	}
	defer s.installing.Store(false)
	if err := SelfUpdateSupport(); err != nil {
		return nil, err
	}
	// Re-check rather than trust the cached status: the button may have been
	// sitting on a page opened an hour ago.
	if _, err := s.Status(ctx, true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	target := s.target
	s.mu.Unlock()
	if target == nil {
		return nil, errors.New("当前已是最新版本")
	}

	binary := findAsset(*target, assetPrefix+runtime.GOARCH)
	if binary == nil {
		return nil, fmt.Errorf("发布 %s 没有 %s 架构的二进制文件", target.TagName, runtime.GOARCH)
	}
	sum, err := s.expectedSum(ctx, *target, binary.Name)
	if err != nil {
		return nil, err
	}
	tmp, err := s.download(ctx, *binary, sum)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)

	reported, err := runVersion(ctx, tmp)
	if err != nil {
		return nil, fmt.Errorf("下载的 %s 无法在本机运行：%w", target.TagName, err)
	}
	if err := os.Rename(tmp, platform.BinPath); err != nil {
		return nil, fmt.Errorf("替换 %s 失败：%w", platform.BinPath, err)
	}
	if err := platform.FinishUpdate(ctx, s.logPath); err != nil {
		return nil, fmt.Errorf("新版本已安装，但重启服务失败：%w（请执行 systemctl restart free-proxy）", err)
	}
	return map[string]any{
		"version": target.TagName, "previous": s.current,
		"reported_version": reported, "restarting": true,
	}, nil
}

// expectedSum reads the release's SHA256SUMS and returns the digest recorded
// for one asset.
func (s *Service) expectedSum(ctx context.Context, r ghRelease, asset string) (string, error) {
	sums := findAsset(r, checksumAsset)
	if sums == nil {
		return "", fmt.Errorf("发布 %s 缺少 %s 校验文件", r.TagName, checksumAsset)
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	body, err := s.fetch(ctx, sums.URL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		digest, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || strings.TrimLeft(strings.TrimSpace(name), "*") != asset {
			continue
		}
		if len(digest) != sha256.Size*2 {
			break
		}
		return strings.ToLower(digest), nil
	}
	return "", fmt.Errorf("%s 中没有 %s 的校验值", checksumAsset, asset)
}

// download streams an asset into a temporary file beside the installed binary
// (the same filesystem, so the later rename is atomic) and refuses to keep one
// whose contents do not match the release's checksum.
func (s *Service) download(ctx context.Context, asset ghAsset, wantSum string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	body, err := s.fetch(ctx, asset.URL)
	if err != nil {
		return "", err
	}
	defer body.Close()

	f, err := os.CreateTemp(filepath.Dir(platform.BinPath), ".free-proxy-update-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, hash), body)
	chmodErr := f.Chmod(0o755)
	closeErr := f.Close()
	if err := errors.Join(copyErr, chmodErr, closeErr); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("下载 %s 失败：%w", asset.Name, err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantSum {
		os.Remove(name)
		return "", fmt.Errorf("%s 校验失败（期望 %s，实际 %s）", asset.Name, wantSum[:12], got[:12])
	}
	return name, nil
}

// fetch performs a GET against a release asset URL. Only github.com is
// accepted, so a compromised or unexpected API response cannot point the
// download at somewhere else.
func (s *Service) fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	if !strings.HasPrefix(url, "https://github.com/") {
		return nil, fmt.Errorf("拒绝从非 GitHub 地址下载：%s", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "free-proxy-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载失败：GitHub 返回 %s", resp.Status)
	}
	return resp.Body, nil
}

// runVersion executes a downloaded binary before it replaces the running one.
// It is the check that catches a build for the wrong libc, the wrong CPU, or a
// truncated file that still hashed correctly — while there is still a working
// binary in place to fall back on.
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return "", fmt.Errorf("%w: %s", err, firstLine(text))
		}
		return "", err
	}
	if text == "" {
		return "", errors.New("`version` 没有任何输出")
	}
	return firstLine(text), nil
}

// tailLog returns the end of the last update's output, or "" when this install
// has never updated itself.
func (s *Service) tailLog() string {
	if s.logPath == "" {
		return ""
	}
	f, err := os.Open(s.logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if offset := info.Size() - logTailBytes; offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	return strings.Join(lines, "\n")
}

func findAsset(r ghRelease, name string) *ghAsset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}
