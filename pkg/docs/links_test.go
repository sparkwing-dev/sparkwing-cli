package docs

import "testing"

// TestRead_RewritesCrossDocLinks asserts that markdown links to
// known topic .md files are rewritten into actionable
// `sparkwing docs read --topic <slug>` commands when an agent or
// human reads a topic via the CLI. Unknown-slug links (typos, files
// outside the topic set) are left unchanged so the CLI doesn't lie
// about what the verb resolves to.
func TestRead_RewritesCrossDocLinks(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		wantSame bool // when true, no rewrite expected
	}{
		{
			name: "labeled link to known topic",
			in:   "See [Cache](gitcache.md) for endpoints.",
			want: "See Cache (`sparkwing docs read --topic gitcache`) for endpoints.",
		},
		{
			name: "filename-as-text collapses to bare command",
			in:   "see [pipelines.md](pipelines.md) for the tour.",
			want: "see `sparkwing docs read --topic pipelines` for the tour.",
		},
		{
			name: "slug-as-text also collapses",
			in:   "Read [pipelines](pipelines.md).",
			want: "Read `sparkwing docs read --topic pipelines`.",
		},
		{
			name: "anchor is dropped (CLI verb has no fragment support)",
			in:   "See [token model](auth.md#tokens) below.",
			want: "See token model (`sparkwing docs read --topic auth`) below.",
		},
		{
			name:     "unknown slug is left alone",
			in:       "See [Foo](nonexistent-topic.md) for details.",
			wantSame: true,
		},
		{
			name:     "external https link is left alone",
			in:       "Visit [the site](https://sparkwing.dev/docs).",
			wantSame: true,
		},
		{
			name:     "fenced code block contents still get rewritten (regex doesn't know about code fences)",
			in:       "```\n[Auth](auth.md)\n```",
			want:     "```\nAuth (`sparkwing docs read --topic auth`)\n```",
			wantSame: false,
		},
		{
			name: "subdir-nested topic",
			in:   "See [the design](design/approval-gates.md).",
			want: "See the design (`sparkwing docs read --topic design/approval-gates`).",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteCLILinks(tc.in)
			if tc.wantSame {
				if got != tc.in {
					t.Fatalf("expected unchanged input %q, got %q", tc.in, got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("rewriteCLILinks(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRead_ActuallyAppliesTransform asserts the transform fires on
// the public Read() entrypoint (not just the internal helper). Uses
// a real cross-link in pipelines.md as the canary -- if the rewrite
// stops being applied, this test fails before users see broken CLI
// output.
func TestRead_ActuallyAppliesTransform(t *testing.T) {
	body, err := Read("pipelines")
	if err != nil {
		t.Fatalf("Read(pipelines): %v", err)
	}
	// pipelines.md links to scheduling.md, hooks.md, api.md. After
	// the transform any of them should appear as a CLI verb.
	wantSubstring := "`sparkwing docs read --topic"
	if !contains(body, wantSubstring) {
		t.Fatalf("Read(pipelines) body missing CLI-verb cross-link substring %q", wantSubstring)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
