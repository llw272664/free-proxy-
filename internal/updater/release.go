// Package updater keeps an installed Free Proxy current from the web console.
// It reads the project's GitHub releases, turns each one into a readable list of
// what changed, and installs a newer build in place — the same work install.sh
// does over SSH, minus the SSH.
//
// Everything user-facing in this package is written in Chinese: its only
// consumer is the console, and these strings are shown to the operator as-is.
package updater

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Section is one group of changes ("新功能", "问题修复", ...) as the console
// renders it. Release notes arrive as markdown and leave as these.
type Section struct {
	Title string   `json:"title"`
	Items []string `json:"items"`
}

// Release is one published version and what it changed.
type Release struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url"`
	Sections    []Section `json:"sections"`
}

// The section titles both note sources produce. scripts/release-notes.sh writes
// these same headings into the release body, so a release built by CI and one
// reconstructed from commits render identically.
const (
	sectionFeature  = "新功能"
	sectionFix      = "问题修复"
	sectionPerf     = "性能优化"
	sectionRefactor = "结构调整"
	sectionDocs     = "文档"
	sectionOther    = "其他改动"
)

// sectionOrder is the order sections are shown in. Anything the release body
// names that is not in this list keeps its own position after these.
var sectionOrder = []string{sectionFeature, sectionFix, sectionPerf, sectionRefactor, sectionDocs, sectionOther}

// commitTypes maps conventional-commit types onto sections. Types absent here
// (and subjects with no type at all) fall into 其他改动 — an unrecognised commit
// is still a change the operator is about to install.
var commitTypes = map[string]string{
	"feat": sectionFeature, "fix": sectionFix, "perf": sectionPerf,
	"refactor": sectionRefactor, "docs": sectionDocs,
	"chore": sectionOther, "build": sectionOther, "ci": sectionOther,
	"style": sectionOther, "test": sectionOther, "revert": sectionOther,
}

// version is a parsed release tag. Tags are vMAJOR.MINOR.PATCH; a build from a
// commit after a tag carries a `-N-gSHA` suffix that describes the distance
// rather than the version, and is dropped here.
type version struct{ major, minor, patch int }

// parseVersion reads a release tag. It fails on "dev" and on bare commit
// hashes — builds that cannot be ordered against a release, which the caller
// reports as an unknown current version rather than treating as old or new.
func parseVersion(tag string) (version, bool) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return version{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return version{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		out[i] = n
	}
	return version{out[0], out[1], out[2]}, true
}

// newer reports whether a is a later version than b.
func (a version) newer(b version) bool {
	switch {
	case a.major != b.major:
		return a.major > b.major
	case a.minor != b.minor:
		return a.minor > b.minor
	default:
		return a.patch > b.patch
	}
}

// parseNotes reads sections out of a release body. GitHub's own generated body
// is a single "Full Changelog" link with no list items, so an empty result is
// the signal to reconstruct the notes from the commit range instead.
func parseNotes(body string) []Section {
	var out []Section
	current := -1
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#"):
			title := strings.TrimSpace(strings.Trim(line, "# "))
			if title == "" {
				continue
			}
			out = append(out, Section{Title: title})
			current = len(out) - 1
		case strings.HasPrefix(line, "- "), strings.HasPrefix(line, "* "):
			item := cleanItem(line[2:])
			if item == "" {
				continue
			}
			if current < 0 {
				out = append(out, Section{Title: sectionOther})
				current = len(out) - 1
			}
			out[current].Items = append(out[current].Items, item)
		}
	}
	return nonEmpty(out)
}

// groupCommits sorts commit subjects into the same sections a release body
// carries, for releases whose body says nothing.
func groupCommits(subjects []string) []Section {
	buckets := map[string][]string{}
	var seen []string
	for _, subject := range subjects {
		title, text := classify(subject)
		if text == "" {
			continue
		}
		if _, ok := buckets[title]; !ok {
			seen = append(seen, title)
		}
		buckets[title] = append(buckets[title], text)
	}
	sort.SliceStable(seen, func(i, j int) bool { return sectionRank(seen[i]) < sectionRank(seen[j]) })
	out := make([]Section, 0, len(seen))
	for _, title := range seen {
		out = append(out, Section{Title: title, Items: buckets[title]})
	}
	return out
}

// classify splits a conventional-commit subject into its section and its text.
// A subject with no recognised type keeps its whole text: "Update README.md" is
// still readable, and guessing at it would only lose the colon.
func classify(subject string) (string, string) {
	subject = strings.TrimSpace(subject)
	head, rest, ok := strings.Cut(subject, ":")
	if !ok {
		return sectionOther, subject
	}
	if i := strings.IndexAny(head, "(!"); i >= 0 {
		head = head[:i]
	}
	title, known := commitTypes[strings.ToLower(strings.TrimSpace(head))]
	if !known {
		return sectionOther, subject
	}
	text := strings.TrimSpace(rest)
	if text == "" {
		return sectionOther, subject
	}
	return title, text
}

// cleanItem strips what GitHub's generated notes append to a line ("by @user in
// <pr url>") and any conventional-commit type still on the front, leaving the
// sentence the section heading already categorised.
func cleanItem(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " by @"); i > 0 {
		s = s[:i]
	}
	if _, text := classify(s); text != "" {
		s = text
	}
	return strings.TrimSpace(s)
}

func sectionRank(title string) int {
	for i, known := range sectionOrder {
		if known == title {
			return i
		}
	}
	return len(sectionOrder)
}

func nonEmpty(sections []Section) []Section {
	out := sections[:0]
	for _, s := range sections {
		if len(s.Items) > 0 {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
