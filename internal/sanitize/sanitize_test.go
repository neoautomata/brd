package sanitize

import (
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
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
			name:         "preserves CKEditor colors fonts borders and spacing",
			input:        `<p style="color: rgb(10, 20, 30); background-color: rgba(255, 255, 0, 0.5); font-size: 48px; font-family: Arial, Helvetica, sans-serif; border: 1px solid hsl(0, 0%, 20%); margin-left: 40px; padding: 4px 8px;">Styled</p><span style="color: hsla(120, 50%, 40%, 0.8); font-size: 1.5em; text-decoration: underline;">Span</span>`,
			wantContains: []string{`rgb(10, 20, 30)`, `rgba(255, 255, 0, 0.5)`, `font-size: 48px`, `font-family: Arial, Helvetica, sans-serif`, `border: 1px solid hsl(0, 0%, 20%)`, `margin-left: 40px`, `padding: 4px 8px`, `hsla(120, 50%, 40%, 0.8)`, `font-size: 1.5em`, `text-decoration: underline`},
		},
		{
			name:         "preserves single- and double-quoted CKEditor font-family lists",
			input:        `<span style="font-family:'Times New Roman', Times, serif;">Serif</span><span style='font-family:"Courier New", Courier, monospace;'>Mono</span><span style="font-family:'Unclosed;">Bad1</span><span style="font-family:'';">Bad2</span>`,
			wantContains: []string{`style="font-family: &#39;Times New Roman&#39;, Times, serif">Serif</span>`, `style="font-family: &#34;Courier New&#34;, Courier, monospace">Mono</span>`, `<span>Bad1</span>`, `<span>Bad2</span>`},
			wantAbsent:   []string{"Unclosed"},
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
			name:         "strips javascript mixed-case vbscript and empty-host href",
			input:        `<a href="JaVaScRiPt:alert(1)">A</a><a href="vbscript:alert(1)">B</a><a href="https:///nohost">C</a><a href="http://">D</a><a href="mailto:">E</a><a href="https://example.com">Valid</a>`,
			wantContains: []string{`<a>A</a>`, `<a>B</a>`, `<a>C</a>`, `<a>D</a>`, `<a>E</a>`, `<a href="https://example.com">Valid</a>`},
			wantAbsent:   []string{"javascript:", "vbscript:", "nohost"},
		},
		{
			name:         "allows mailto and fragment href",
			input:        `<a href="mailto:user@example.com">Mail</a><a href="#section-1">Jump</a>`,
			wantContains: []string{`<a href="mailto:user@example.com">Mail</a>`, `<a href="#section-1">Jump</a>`},
		},
		{
			name:         "blocks protocol-relative href",
			input:        `<a href="//evil.example/phish">Phish</a><a href="/local">Local</a>`,
			wantContains: []string{`<a>Phish</a>`, `<a href="/local">Local</a>`},
			wantAbsent:   []string{"evil.example"},
		},
		{
			name:         "blocks backslash and control characters in href and img src",
			input:        `<a href="/\evil.example/phish">BadLink</a><img src="/\evil.example/a.png"><a href="/ok` + "\n" + `">Ctrl</a>`,
			wantContains: []string{`<a>BadLink</a>`, `<img>`, `<a>Ctrl</a>`},
			wantAbsent:   []string{"evil.example", `/\`},
		},
		{
			name:         "handles target and rel normalization on anchor",
			input:        `<a href="https://example.com" target="_blank">Ext</a><a href="https://example.com" target="_self" rel="opener">Self</a><a href="https://example.com" target="_top">Top</a>`,
			wantContains: []string{`<a href="https://example.com" target="_blank" rel="noopener noreferrer">Ext</a>`, `target="_self"`, `rel="noopener noreferrer"`, `<a href="https://example.com">Top</a>`},
			wantAbsent:   []string{`target="_top"`, `rel="opener"`},
		},
		{
			name:       "strips dangerous style expressions url image-set calc var comments braces newlines and escapes",
			input:      `<span style="background-image: url(javascript:alert(1)); color: red;">A</span><span style="color: /*comment*/ red">B</span><span style="color: \75rl(x)">C</span><span style="color: red} body{display:none">D</span><span style="background-color: image-set(x)">E</span><span style="color: red` + "\n" + `background-color: blue">F</span><span style="width: calc(100% - 10px); color: green">G</span><span style="color: var(--x); text-align: left">H</span>`,
			wantAbsent: []string{"url(", "javascript:", "/*", `\75`, "display:none", "image-set", "blue", "calc(", "var(", "green"},
		},
		{
			name:         "rejects negative margins and huge sizes while keeping valid props",
			input:        `<p style="margin-left: -9999px; font-size: 9999px; width: 50000px; padding: 999px; text-align: center;">Safe</p>`,
			wantContains: []string{`<p style="text-align: center">Safe</p>`},
			wantAbsent:   []string{"-9999", "9999px", "50000", "999px"},
		},
		{
			name:         "drops nested svg subtrees cleanly without leaking or eating trailing text",
			input:        `<svg><p>x</svg>after1<svg></a>leaked</svg>after2`,
			wantContains: []string{"after1", "after2"},
			wantAbsent:   []string{"<svg", "leaked"},
		},
		{
			name:         "drops dangerous subtrees completely",
			input:        `<title>t1</title><xmp><raw></xmp><audio>a</audio><video>v</video><noframes>nf</noframes><noscript>ns</noscript><template><p>tpl</p></template><form><p>fm</p></form><style>body{color:red}</style><math><mi>m</mi></math><textarea>ta</textarea><embed src="x"><applet>ap</applet><button>btn</button><select><option>opt</option></select><noembed>ne</noembed><p>Keep</p>`,
			wantContains: []string{"<p>Keep</p>"},
			wantAbsent:   []string{"t1", "raw", "<audio", "<video", "nf", "ns", "tpl", "fm", "color:red", "<math", "ta", "<embed", "ap", "btn", "opt", "ne"},
		},
		{
			name:         "drops plaintext subtree",
			input:        `<p>Before</p><plaintext>hidden`,
			wantContains: []string{"<p>Before</p>"},
			wantAbsent:   []string{"hidden", "<plaintext"},
		},
		{
			name:         "balances stray closing tags without breaking host layout",
			input:        `</div></div><p>text</p>`,
			wantContains: []string{"<p>text</p>"},
			wantAbsent:   []string{"</div>"},
		},
		{
			name:         "allows valid base64 raster images and blocks invalid or svg data urls",
			input:        `<img src="data:image/png;base64,iVBORw0KGgo="><img src="data:image/jpeg;base64,/9j/4AAQ"><img src="data:image/gif;base64,R0lGODlh"><img src="data:image/webp;base64,UklGRg=="><img src="data:image/png;base64,not*valid!"><img src="data:image/png;base64,"><img src="data:image/svg+xml;base64,PHN2Zz4=">`,
			wantContains: []string{`data:image/png;base64,iVBORw0KGgo=`, `data:image/jpeg;base64,/9j/4AAQ`, `data:image/gif;base64,R0lGODlh`, `data:image/webp;base64,UklGRg==`},
			wantAbsent:   []string{"not*valid", "svg+xml", `src="data:image/png;base64,"`},
		},
		{
			name:         "hardens img src against SSRF",
			input:        `<img src="https://example.com/ok.png"><img src="http://example.com/plain.png"><img src="/local.png"><img src="https:///emptyhost.png"><img src="https://localhost/a.png"><img src="https://a.localhost/a.png"><img src="https://printer.local/a.png"><img src="https://metadata.google.internal/a.png"><img src="https://intranet/a.png"><img src="https://127.0.0.1/a.png"><img src="https://[::1]/a.png"><img src="https://169.254.169.254/latest"><img src="https://100.64.0.1/a.png"><img src="https://10.0.0.1/a.png"><img src="https://192.168.1.1/a.png"><img src="https://8.8.8.8/a.png"><img src="https://0177.0.0.1/a.png"><img src="https://2130706433/a.png"><img src="https://user@example.com/u.png">`,
			wantContains: []string{`<img src="https://example.com/ok.png">`},
			wantAbsent:   []string{"plain.png", "/local.png", "emptyhost", "localhost", ".local", ".internal", "intranet", "127.0.0.1", "::1", "169.254", "100.64", "10.0.0.1", "192.168", "8.8.8.8", "0177", "2130706433", "user@"},
		},
		{
			name:         "allows checkbox input for todo lists and blocks text input",
			input:        `<input type="checkbox" disabled checked><input type="text" value="x">`,
			wantContains: []string{`<input type="checkbox" disabled="disabled" checked="checked">`},
			wantAbsent:   []string{`type="text"`},
		},
		{
			name:         "scopes attributes strictly to valid tags and bounds numeric attributes",
			input:        `<p alt="x" colspan="2" rowspan="2" start="3" reversed checked disabled width="100" height="50">P</p><img src="https://example.com/a.png" alt="Photo" width="200px" height="50%"><img src="https://example.com/b.png" width="9999px" height="500%" width="-1"><table width="100%"><tr><td colspan="2" rowspan="50" width="25%">A</td><th colspan="51" rowspan="0" height="0">B</th></tr></table><ol start="5" reversed><li>One</li></ol><ol start="999999"><li>Two</li></ol>`,
			wantContains: []string{`<p>P</p>`, `<img src="https://example.com/a.png" alt="Photo" width="200px" height="50%">`, `<img src="https://example.com/b.png">`, `<table width="100%">`, `<td colspan="2" rowspan="50" width="25%">A</td>`, `<th>B</th>`, `<ol start="5" reversed="reversed"><li>One</li></ol>`, `<ol><li>Two</li></ol>`},
			wantAbsent:   []string{`9999px`, `500%`, `colspan="51"`, `rowspan="0"`, `start="999999"`},
		},
		{
			name:         "filters class tokens and strips comments and unwraps unknown tags",
			input:        `<!-- secret comment --><p class="ck-list good_1 bad!class another-ok">Hi</p><custom-widget>Unwrapped</custom-widget>`,
			wantContains: []string{`<p class="ck-list good_1 another-ok">Hi</p>`, `Unwrapped`},
			wantAbsent:   []string{"secret comment", "bad!class", "custom-widget"},
		},
		{
			name:         "deduplicates repeated attributes keeping first valid value",
			input:        `<a href="https://first.example" href="https://second.example" title="First" title="Second">Link</a>`,
			wantContains: []string{`<a href="https://first.example" title="First">Link</a>`},
			wantAbsent:   []string{"second.example", "Second"},
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

func TestDropsForeignNamespaceNodesAndAttributes(t *testing.T) {
	t.Run("foreign namespace element dropped", func(t *testing.T) {
		node := &xhtml.Node{
			Type:      xhtml.ElementNode,
			Namespace: "svg",
			Data:      "a",
			Attr:      []xhtml.Attribute{{Key: "href", Val: "https://example.com"}},
		}
		node.AppendChild(&xhtml.Node{Type: xhtml.TextNode, Data: "foreign"})

		var out strings.Builder
		renderNode(&out, node)
		if out.String() != "" {
			t.Errorf("foreign namespace element rendered %q, want empty", out.String())
		}
	})

	t.Run("foreign namespace attribute dropped", func(t *testing.T) {
		attrs := filterAttributes("a", []xhtml.Attribute{
			{Namespace: "xlink", Key: "href", Val: "javascript:alert(1)"},
			{Key: "href", Val: "https://example.com"},
		})
		if len(attrs) != 1 || attrs[0].Key != "href" || attrs[0].Val != "https://example.com" {
			t.Errorf("unexpected filtered attrs: %+v", attrs)
		}
	})
}
