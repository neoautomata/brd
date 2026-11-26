package sanitize

import (
	"strings"
	"testing"
)

func TestHTML(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "preserves CKEditor formatting",
			input:        `<h2 style="text-align: center; color: #000;">Notes</h2><p><strong>Milk</strong> &amp; <em>Bread</em></p><ul><li>Item 1</li></ul>`,
			wantContains: []string{`<h2 style="text-align: center; color: #000">Notes</h2>`, `<strong>Milk</strong>`, `<em>Bread</em>`, `<ul><li>Item 1</li></ul>`},
		},
		{
			name:       "strips script tags and body",
			input:      `<p>Hello</p><script>alert(1)</script><p>World</p>`,
			wantAbsent: []string{"<script", "alert(1)"},
		},
		{
			name:       "strips iframe and object",
			input:      `<iframe src="https://evil.example">fallback</iframe><object data="x"></object><p>Safe</p>`,
			wantAbsent: []string{"<iframe", "<object", "fallback"},
		},
		{
			name:         "strips event handler attributes",
			input:        `<p onclick="alert(1)" onmouseover="evil()">Text</p><img src="https://example.com/a.png" onerror="alert(2)">`,
			wantContains: []string{`<p>Text</p>`, `<img src="https://example.com/a.png">`},
			wantAbsent:   []string{"onclick", "onmouseover", "onerror", "alert"},
		},
		{
			name:         "strips javascript mixed-case and vbscript href",
			input:        `<a href="JaVaScRiPt:alert(1)">A</a><a href="vbscript:alert(1)">B</a><a href="https://example.com">Valid</a>`,
			wantContains: []string{`<a>A</a>`, `<a>B</a>`, `<a href="https://example.com">Valid</a>`},
			wantAbsent:   []string{"javascript:", "vbscript:"},
		},
		{
			name:         "blocks protocol-relative href",
			input:        `<a href="//evil.example/phish">Phish</a><a href="/local">Local</a>`,
			wantContains: []string{`<a>Phish</a>`, `<a href="/local">Local</a>`},
			wantAbsent:   []string{"evil.example"},
		},
		{
			name:         "blocks backslash and control characters in href and img src",
			input:        `<a href="/\evil.example/phish">BadLink</a><img src="/\evil.example/a.png"><a href="/ok\n">Ctrl</a>`,
			wantContains: []string{`<a>BadLink</a>`, `<img>`},
			wantAbsent:   []string{"evil.example", `/\`},
		},
		{
			name:         "adds rel noopener noreferrer when target is _blank",
			input:        `<a href="https://example.com" target="_blank">External</a>`,
			wantContains: []string{`target="_blank"`, `rel="noopener noreferrer"`},
		},
		{
			name:       "strips dangerous style expressions url comments and escapes",
			input:      `<span style="background-image: url(javascript:alert(1)); color: red;">A</span><span style="color: /*comment*/ red">B</span><span style="color: \75rl(x)">C</span>`,
			wantAbsent: []string{"url(", "javascript:", "/*", `\75`},
		},
		{
			name:         "drops nested svg subtrees cleanly without leaking or eating trailing text",
			input:        `<svg><p>x</svg>after1<svg></a>leaked</svg>after2`,
			wantContains: []string{"after1", "after2"},
			wantAbsent:   []string{"<svg", "leaked"},
		},
		{
			name:         "drops title xmp audio video noframes subtrees completely",
			input:        `<title>secret</title><xmp><raw></xmp><audio>a</audio><video>v</video><noframes>nf</noframes><p>Keep</p>`,
			wantContains: []string{"<p>Keep</p>"},
			wantAbsent:   []string{"secret", "raw", "<audio", "<video", "nf"},
		},
		{
			name:         "balances stray closing tags without breaking host layout",
			input:        `</div></div><p>text</p>`,
			wantContains: []string{"<p>text</p>"},
			wantAbsent:   []string{"</div>"},
		},
		{
			name:         "allows base64 raster images and blocks svg data urls",
			input:        `<img src="data:image/png;base64,iVBORw0KGgo="><img src="data:image/svg+xml;base64,PHN2Zz4=">`,
			wantContains: []string{`data:image/png;base64,iVBORw0KGgo=`},
			wantAbsent:   []string{"svg+xml"},
		},
		{
			name:         "allows checkbox input for todo lists and blocks text input",
			input:        `<input type="checkbox" disabled checked><input type="text" value="x">`,
			wantContains: []string{`type="checkbox"`},
			wantAbsent:   []string{`type="text"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HTML(tc.input)
			for _, sub := range tc.wantContains {
				if !strings.Contains(got, sub) {
					t.Errorf("HTML() = %q, expected to contain %q", got, sub)
				}
			}
			for _, sub := range tc.wantAbsent {
				if strings.Contains(strings.ToLower(got), strings.ToLower(sub)) {
					t.Errorf("HTML() = %q, expected NOT to contain %q", got, sub)
				}
			}
		})
	}
}
