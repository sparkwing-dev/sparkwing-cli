// Package docs ships the sparkwing user-facing documentation as
// embedded markdown so the CLI can answer doc questions offline,
// version-locked to the running binary.
//
// Embedding philosophy: docs are part of the product, not a separate
// asset to fetch. An agent running `sparkwing docs read --topic
// pipelines` should always get the docs that match the CLI version
// it's looking at -- no risk of "the website explained a flag that
// doesn't exist on this binary yet."
//
// Source of truth is repo-root /docs/. Per-file size budget is
// negligible (~300KB total versus a 60MB binary), so we embed the
// whole tree without filtering. The marketing site at
// sparkwing.dev pulls from the same /docs/ tree at build time, so
// there is one source for both surfaces.
package docs

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// allDocs is the embedded /docs/ subtree. Files are accessed by
// their slug (filename minus .md, plus relative subpath for design/).
// We embed the full tree -- including design/ subdir -- because some
// docs cross-link there and an agent reading "all" should see the
// full corpus, not a filtered subset.
//
//go:embed all:content
var allDocs embed.FS

// Entry describes one doc topic. Slug is what the CLI takes via
// --topic; Path is the on-disk relative path (used by the website's
// build to reproduce the same layout). Title and Summary are
// extracted from the markdown's first H1 / first paragraph so an
// agent can scan `docs list --json` for relevance without slurping
// every body.
type Entry struct {
	Slug    string `json:"slug"`
	Path    string `json:"path"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Bytes   int    `json:"bytes"`
}

// List returns every embedded doc in alphabetical slug order. The
// list is sorted so callers (table renderer, llms.txt generator) get
// a stable ordering without re-sorting.
func List() []Entry {
	var entries []Entry
	_ = fs.WalkDir(allDocs, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		body, rerr := fs.ReadFile(allDocs, p)
		if rerr != nil {
			return nil
		}
		rel := strings.TrimPrefix(p, "content/")
		slug := strings.TrimSuffix(rel, ".md")
		title, summary := extractTitleSummary(body)
		entries = append(entries, Entry{
			Slug:    slug,
			Path:    rel,
			Title:   title,
			Summary: summary,
			Bytes:   len(body),
		})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries
}

// Read returns the markdown body for the given slug, with cross-doc
// markdown links rewritten into actionable CLI commands. Slug is
// case-sensitive and matches the path layout under /docs/ minus the
// trailing .md. Returns ErrNotFound when the slug is unknown so
// callers can render a useful suggestion list.
//
// CLI link rewriting: see rewriteCLILinks. The rewrite makes
// `[Cache](gitcache.md)` show up as “Cache (`sparkwing docs read
// --topic gitcache`)“ in the CLI -- closing the discoverability gap
// between docs without requiring authors to write CLI-flavored links
// in the source. The website's renderer reads the markdown source
// directly (not via this function) so its hyperlinks are unaffected.
func Read(slug string) (string, error) {
	slug = strings.TrimSuffix(slug, ".md")
	p := path.Join("content", slug+".md")
	body, err := fs.ReadFile(allDocs, p)
	if err != nil {
		return "", fmt.Errorf("docs: %q: %w", slug, ErrNotFound)
	}
	return rewriteCLILinks(string(body)), nil
}

// crossDocLinkPattern matches markdown links to a *.md file, with an
// optional fragment (#anchor). The path group ([^)#]+) excludes `)`
// and `#` so anchors stop the path capture; the path can contain
// slashes (e.g. `design/foo.md`) so subdir-nested topics work.
var crossDocLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)#]+)\.md(?:#[^)]*)?\)`)

// rewriteCLILinks transforms `[text](slug.md)` markdown links into
// `sparkwing docs read --topic <slug>` form when slug is a known
// topic. Unknown slugs (typos, external markdown files, design
// drafts) are left unchanged so the CLI dump doesn't lie about what
// the verb resolves to.
//
// Two output shapes:
//   - Link text equals the filename ("[pipelines](pipelines.md)" or
//     "[pipelines.md](pipelines.md)") -> bare “sparkwing docs read
//     --topic pipelines“ . The original text was redundant with the
//     filename anyway.
//   - Link text differs ("[Cache](gitcache.md)") -> the original text
//     is preserved with the command appended:
//     “Cache (sparkwing docs read --topic gitcache)“.
//
// Anchors (`#section`) are dropped because `sparkwing docs read`
// doesn't take a fragment; the section is still findable by
// scrolling within the resulting topic.
//
// Source markdown stays unchanged on disk -- the website's renderer
// reads from `content/docs/` (sparkwing-product) or from the parsed
// markdown source directly and produces real hyperlinks. Only the
// CLI's `Read()` path applies this transform.
func rewriteCLILinks(body string) string {
	knownSlugs := make(map[string]struct{})
	for _, e := range List() {
		knownSlugs[e.Slug] = struct{}{}
	}
	return crossDocLinkPattern.ReplaceAllStringFunc(body, func(match string) string {
		m := crossDocLinkPattern.FindStringSubmatch(match)
		if len(m) != 3 {
			return match
		}
		text, slug := m[1], m[2]
		if _, ok := knownSlugs[slug]; !ok {
			return match
		}
		cmd := "`sparkwing docs read --topic " + slug + "`"
		if text == slug || text == slug+".md" {
			return cmd
		}
		return text + " (" + cmd + ")"
	})
}

// All returns every doc concatenated with a small ASCII separator
// between each. Designed for `sparkwing docs all` -- one stdout dump
// an agent can slurp via Bash without making N requests. Order
// matches List() (alphabetical by slug).
func All() string {
	var b strings.Builder
	for _, e := range List() {
		body, err := Read(e.Slug)
		if err != nil {
			continue
		}
		b.WriteString("\n========================================\n")
		b.WriteString("# DOC: ")
		b.WriteString(e.Slug)
		b.WriteByte('\n')
		b.WriteString("========================================\n\n")
		b.WriteString(strings.TrimSpace(body))
		b.WriteString("\n")
	}
	return b.String()
}

// Search returns entries whose title, slug, or body contain every
// space-separated token in query (case-insensitive). Order: hits in
// title/slug rank above body-only matches. Empty query returns the
// full List().
//
// Deliberately simple substring matching -- not BM25 / not stemmed.
// An agent's query is usually exact-keyword ("approval", "warm pool")
// and the corpus is small (~25 files); fancy ranking would add
// complexity without measurable benefit at this scale.
func Search(query string) []Entry {
	query = strings.TrimSpace(query)
	if query == "" {
		return List()
	}
	tokens := strings.Fields(strings.ToLower(query))
	type scored struct {
		Entry
		score int
	}
	var hits []scored
	for _, e := range List() {
		body, err := Read(e.Slug)
		if err != nil {
			continue
		}
		hay := strings.ToLower(e.Title + " " + e.Slug + " " + body)
		titleHay := strings.ToLower(e.Title + " " + e.Slug)

		var score int
		all := true
		for _, tok := range tokens {
			if !strings.Contains(hay, tok) {
				all = false
				break
			}
			if strings.Contains(titleHay, tok) {
				score += 10
			} else {
				score += 1
			}
		}
		if !all {
			continue
		}
		hits = append(hits, scored{Entry: e, score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].Slug < hits[j].Slug
	})
	out := make([]Entry, len(hits))
	for i, h := range hits {
		out[i] = h.Entry
	}
	return out
}

// ErrNotFound signals an unknown slug. Callers print suggestions
// (`docs list`) when they receive this.
type docsError string

func (e docsError) Error() string { return string(e) }

const ErrNotFound = docsError("doc not found")

// extractTitleSummary pulls the first H1 (`# Title`) as Title and
// the first non-empty paragraph after it as Summary. Skips
// blockquotes (status banners like "> STATUS: stale...") so the
// summary is the actual descriptive text. Falls back gracefully on
// docs without an H1 (uses the first non-empty line) or without a
// summary paragraph (empty Summary, agent can fetch the body).
func extractTitleSummary(body []byte) (title, summary string) {
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	state := 0 // 0: looking for title, 1: looking for summary, 2: collecting summary
	var summaryLines []string
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		switch state {
		case 0:
			if strings.HasPrefix(trim, "# ") {
				title = strings.TrimSpace(strings.TrimPrefix(trim, "#"))
				state = 1
			} else if title == "" && trim != "" && !strings.HasPrefix(trim, "<!--") {
				title = trim
			}
		case 1:
			if trim == "" || strings.HasPrefix(trim, "<!--") || strings.HasPrefix(trim, ">") {
				continue
			}
			summaryLines = append(summaryLines, trim)
			state = 2
		case 2:
			if trim == "" {
				return title, strings.Join(summaryLines, " ")
			}
			if strings.HasPrefix(trim, "#") {
				return title, strings.Join(summaryLines, " ")
			}
			summaryLines = append(summaryLines, trim)
		}
	}
	return title, strings.Join(summaryLines, " ")
}
