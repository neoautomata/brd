package sanitize

import (
	"html"
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var allowedTags = map[string]bool{
	"a": true, "b": true, "blockquote": true, "br": true, "code": true,
	"col": true, "colgroup": true, "del": true, "div": true, "em": true,
	"figcaption": true, "figure": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "hr": true, "i": true, "img": true,
	"input": true, "label": true, "li": true, "mark": true, "ol": true,
	"p": true, "pre": true, "s": true, "span": true, "strong": true,
	"sub": true, "sup": true, "table": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "tr": true, "u": true, "ul": true,
}

var voidTags = map[string]bool{
	"br": true, "col": true, "hr": true, "img": true, "input": true,
}

var dropSubtreeTags = map[string]bool{
	"applet": true, "button": true, "embed": true, "form": true, "iframe": true,
	"math": true, "noscript": true, "object": true, "script": true, "select": true,
	"style": true, "svg": true, "template": true, "textarea": true,
}

var allowedCSSProps = map[string]bool{
	"background-color": true, "border": true, "border-color": true,
	"border-style": true, "border-width": true, "color": true,
	"font-family": true, "font-size": true, "font-style": true,
	"font-weight": true, "height": true, "list-style-type": true,
	"margin": true, "margin-bottom": true, "margin-left": true,
	"margin-right": true, "margin-top": true, "max-width": true,
	"padding": true, "padding-bottom": true, "padding-left": true,
	"padding-right": true, "padding-top": true, "text-align": true,
	"text-decoration": true, "vertical-align": true, "width": true,
}

// HTML parses rich-text markup into a DOM fragment and serializes only allowlisted nodes.
// It strips unsafe subtrees, attributes, URLs, and styles while preserving balanced CKEditor markup.
func HTML(input string) string {
	ctxNode := &xhtml.Node{
		Type:     xhtml.ElementNode,
		DataAtom: atom.Div,
		Data:     "div",
	}
	nodes, err := xhtml.ParseFragment(strings.NewReader(input), ctxNode)
	if err != nil {
		return html.EscapeString(input)
	}

	var out strings.Builder
	for _, n := range nodes {
		renderNode(&out, n)
	}
	return out.String()
}

func renderNode(out *strings.Builder, n *xhtml.Node) {
	switch n.Type {
	case xhtml.TextNode:
		out.WriteString(html.EscapeString(n.Data))

	case xhtml.ElementNode:
		tag := strings.ToLower(n.Data)
		if dropSubtreeTags[tag] {
			return
		}
		if !allowedTags[tag] {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				renderNode(out, c)
			}
			return
		}

		attrs := filterAttributes(tag, n.Attr)
		if tag == "input" && !isCheckboxInput(attrs) {
			return
		}

		out.WriteByte('<')
		out.WriteString(tag)
		for _, attr := range attrs {
			out.WriteByte(' ')
			out.WriteString(attr.Key)
			out.WriteString(`="`)
			out.WriteString(html.EscapeString(attr.Val))
			out.WriteByte('"')
		}
		out.WriteByte('>')

		if voidTags[tag] {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(out, c)
		}
		out.WriteString("</")
		out.WriteString(tag)
		out.WriteByte('>')
	}
}

func filterAttributes(tag string, attrs []xhtml.Attribute) []xhtml.Attribute {
	var safe []xhtml.Attribute
	seen := make(map[string]bool, len(attrs))
	hasBlankTarget := false
	hasRel := false

	addAttr := func(k, v string) {
		if seen[k] {
			return
		}
		seen[k] = true
		safe = append(safe, xhtml.Attribute{Key: k, Val: v})
	}

	for _, a := range attrs {
		key := strings.ToLower(strings.TrimSpace(a.Key))
		if strings.HasPrefix(key, "on") {
			continue
		}
		val := strings.TrimSpace(a.Val)

		switch key {
		case "class":
			if clean := sanitizeClass(val); clean != "" {
				addAttr("class", clean)
			}
		case "style":
			if clean := sanitizeStyle(val); clean != "" {
				addAttr("style", clean)
			}
		case "href":
			if tag == "a" && isSafeHref(val) {
				addAttr("href", val)
			}
		case "src":
			if tag == "img" && isSafeImgSrc(val) {
				addAttr("src", val)
			}
		case "alt", "title", "colspan", "rowspan", "start", "width", "height":
			addAttr(key, val)
		case "target":
			if tag == "a" && (val == "_blank" || val == "_self") {
				addAttr("target", val)
				if val == "_blank" {
					hasBlankTarget = true
				}
			}
		case "rel":
			if tag == "a" {
				addAttr("rel", "noopener noreferrer")
				hasRel = true
			}
		case "type":
			if tag == "input" && strings.EqualFold(val, "checkbox") {
				addAttr("type", "checkbox")
			}
		case "checked", "disabled", "reversed":
			addAttr(key, key)
		}
	}

	if tag == "a" && hasBlankTarget && !hasRel {
		addAttr("rel", "noopener noreferrer")
	}

	return safe
}

func isCheckboxInput(attrs []xhtml.Attribute) bool {
	for _, a := range attrs {
		if a.Key == "type" && a.Val == "checkbox" {
			return true
		}
	}
	return false
}

func sanitizeClass(raw string) string {
	var parts []string
	for _, tok := range strings.Fields(raw) {
		valid := true
		for _, r := range tok {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_') {
				valid = false
				break
			}
		}
		if valid {
			parts = append(parts, tok)
		}
	}
	return strings.Join(parts, " ")
}

func sanitizeStyle(raw string) string {
	if strings.ContainsRune(raw, '\\') || strings.Contains(raw, "/*") || strings.Contains(raw, "*/") {
		return ""
	}
	lower := strings.ToLower(raw)
	for _, bad := range []string{"expression", "url(", "javascript:", "vbscript:", "@import", "behavior", "-moz-binding"} {
		if strings.Contains(lower, bad) {
			return ""
		}
	}

	var kept []string
	for _, decl := range strings.Split(raw, ";") {
		decl = strings.TrimSpace(decl)
		if decl == "" {
			continue
		}
		colon := strings.IndexByte(decl, ':')
		if colon <= 0 {
			continue
		}
		prop := strings.ToLower(strings.TrimSpace(decl[:colon]))
		val := strings.TrimSpace(decl[colon+1:])
		if !allowedCSSProps[prop] || val == "" {
			continue
		}
		kept = append(kept, prop+": "+val)
	}
	return strings.Join(kept, "; ")
}

func hasUnsafeURLChars(s string) bool {
	for _, r := range s {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isSafeHref(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || hasUnsafeURLChars(trimmed) {
		return false
	}
	if strings.HasPrefix(trimmed, "#") {
		return true
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	if u.Scheme == "" {
		return u.Host == "" && !strings.HasPrefix(trimmed, "//")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		return true
	default:
		return false
	}
}

func isSafeImgSrc(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || hasUnsafeURLChars(trimmed) {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{
		"data:image/png;base64,",
		"data:image/jpeg;base64,",
		"data:image/gif;base64,",
		"data:image/webp;base64,",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	if u.Scheme == "" {
		return u.Host == "" && strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "//")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}
