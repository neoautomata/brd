package sanitize

import (
	"encoding/base64"
	"html"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	maxTableSpan    = 50
	minListStart    = -10000
	maxListStart    = 10000
	maxAttrDimPx    = 2000
	maxAttrDimPct   = 100
	maxFontFamilyLn = 200
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
	"applet": true, "audio": true, "button": true, "embed": true, "form": true,
	"head": true, "iframe": true, "math": true, "noembed": true, "noframes": true,
	"noscript": true, "object": true, "plaintext": true, "script": true, "select": true,
	"style": true, "svg": true, "template": true, "textarea": true, "title": true,
	"video": true, "xmp": true,
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

var (
	textAlignValues = map[string]bool{
		"left": true, "right": true, "center": true, "justify": true,
		"start": true, "end": true, "inherit": true,
	}
	verticalAlignValues = map[string]bool{
		"baseline": true, "sub": true, "super": true, "text-top": true,
		"text-bottom": true, "middle": true, "top": true, "bottom": true,
		"inherit": true,
	}
	fontStyleValues = map[string]bool{
		"normal": true, "italic": true, "oblique": true, "inherit": true,
	}
	fontWeightKeywords = map[string]bool{
		"normal": true, "bold": true, "bolder": true, "lighter": true, "inherit": true,
	}
	fontSizeKeywords = map[string]bool{
		"xx-small": true, "x-small": true, "small": true, "medium": true,
		"large": true, "x-large": true, "xx-large": true,
		"smaller": true, "larger": true, "inherit": true,
	}
	textDecorationKeywords = map[string]bool{
		"none": true, "underline": true, "overline": true, "line-through": true,
		"solid": true, "double": true, "dotted": true, "dashed": true,
		"wavy": true, "inherit": true,
	}
	listStyleTypeValues = map[string]bool{
		"none": true, "disc": true, "circle": true, "square": true,
		"decimal": true, "decimal-leading-zero": true,
		"lower-roman": true, "upper-roman": true,
		"lower-latin": true, "upper-latin": true,
		"lower-alpha": true, "upper-alpha": true,
		"inherit": true,
	}
	borderStyleValues = map[string]bool{
		"none": true, "hidden": true, "dotted": true, "dashed": true,
		"solid": true, "double": true, "groove": true, "ridge": true,
		"inset": true, "outset": true, "inherit": true,
	}
	borderWidthKeywords = map[string]bool{
		"thin": true, "medium": true, "thick": true, "inherit": true,
	}
	sizeKeywords = map[string]bool{
		"auto": true, "inherit": true, "max-content": true,
		"min-content": true, "fit-content": true,
	}
)

// HTML parses rich-text markup into a DOM fragment and serializes only allowlisted nodes.
// HTML strips unsafe subtrees, attributes, URLs, and styles while preserving CKEditor markup.
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
		if n.Namespace != "" {
			return
		}
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
		if a.Namespace != "" {
			continue
		}
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
			if clean := sanitizeStyle(a.Val); clean != "" {
				addAttr("style", clean)
			}
		case "title":
			addAttr("title", val)
		case "href":
			if tag == "a" && isSafeHref(a.Val) {
				addAttr("href", val)
			}
		case "src":
			if tag == "img" && isSafeImgSrc(a.Val) {
				addAttr("src", val)
			}
		case "alt":
			if tag == "img" {
				addAttr("alt", val)
			}
		case "colspan", "rowspan":
			if tag == "td" || tag == "th" {
				if n, ok := parseBoundedInt(val, 1, maxTableSpan); ok {
					addAttr(key, strconv.Itoa(n))
				}
			}
		case "start":
			if tag == "ol" {
				if n, ok := parseBoundedInt(val, minListStart, maxListStart); ok {
					addAttr("start", strconv.Itoa(n))
				}
			}
		case "reversed":
			if tag == "ol" {
				addAttr("reversed", "reversed")
			}
		case "width", "height":
			if tag == "img" || tag == "table" || tag == "td" || tag == "th" {
				if clean, ok := sanitizeDimension(val); ok {
					addAttr(key, clean)
				}
			}
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
		case "checked", "disabled":
			if tag == "input" {
				addAttr(key, key)
			}
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

func parseBoundedInt(raw string, minVal, maxVal int) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < minVal || n > maxVal {
		return 0, false
	}
	return n, true
}

func sanitizeDimension(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", false
	}
	unit := ""
	numStr := s
	switch {
	case strings.HasSuffix(s, "px"):
		unit = "px"
		numStr = strings.TrimSuffix(s, "px")
	case strings.HasSuffix(s, "%"):
		unit = "%"
		numStr = strings.TrimSuffix(s, "%")
	}
	val, ok := parseUnsignedDecimal(numStr)
	if !ok || val <= 0 {
		return "", false
	}
	if unit == "%" {
		if val > maxAttrDimPct {
			return "", false
		}
	} else {
		if val > maxAttrDimPx {
			return "", false
		}
	}
	return numStr + unit, true
}

func parseUnsignedDecimal(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	dotSeen := false
	digitSeen := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			digitSeen = true
			continue
		}
		if c == '.' && !dotSeen {
			dotSeen = true
			continue
		}
		return 0, false
	}
	if !digitSeen {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
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
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '{' || r == '}' || r == '<' || r == '>' {
			return ""
		}
	}
	if strings.Contains(raw, "/*") || strings.Contains(raw, "*/") {
		return ""
	}
	lower := strings.ToLower(raw)
	for _, bad := range []string{
		"expression", "url(", "image-set(", "javascript:", "vbscript:",
		"@import", "behavior", "-moz-binding", "var(", "env(", "calc(",
		"attr(", "counter(", "counters(",
	} {
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
		if !isSafeCSSValue(prop, val) {
			continue
		}
		kept = append(kept, prop+": "+val)
	}
	return strings.Join(kept, "; ")
}

func isSafeCSSValue(prop, val string) bool {
	lower := strings.ToLower(strings.TrimSpace(val))
	if lower == "" {
		return false
	}

	switch prop {
	case "color", "background-color", "border-color":
		return isSafeCSSColor(lower)
	case "text-align":
		return textAlignValues[lower]
	case "vertical-align":
		return verticalAlignValues[lower]
	case "font-style":
		return fontStyleValues[lower]
	case "font-weight":
		if fontWeightKeywords[lower] {
			return true
		}
		_, ok := parseBoundedInt(lower, 1, 1000)
		return ok
	case "list-style-type":
		return listStyleTypeValues[lower]
	case "font-family":
		return isSafeFontFamily(val)
	case "font-size":
		if fontSizeKeywords[lower] {
			return true
		}
		return isSafeCSSLength(lower, false, 1, 200, 1, 150, 0.1, 10, 10, 500)
	case "width", "height", "max-width":
		if sizeKeywords[lower] {
			return true
		}
		return isSafeCSSLength(lower, true, 0, 2000, 0, 1500, 0, 100, 0, 100)
	case "margin":
		return isSafeSpacingList(lower, 4, true)
	case "margin-top", "margin-right", "margin-bottom", "margin-left":
		return isSafeSpacingList(lower, 1, true)
	case "padding":
		return isSafeSpacingList(lower, 4, false)
	case "padding-top", "padding-right", "padding-bottom", "padding-left":
		return isSafeSpacingList(lower, 1, false)
	case "border-style":
		toks, ok := splitCSSTokens(lower)
		if !ok || len(toks) == 0 || len(toks) > 4 {
			return false
		}
		for _, tok := range toks {
			if !borderStyleValues[tok] {
				return false
			}
		}
		return true
	case "border-width":
		toks, ok := splitCSSTokens(lower)
		if !ok || len(toks) == 0 || len(toks) > 4 {
			return false
		}
		for _, tok := range toks {
			if !isSafeBorderWidthToken(tok) {
				return false
			}
		}
		return true
	case "border":
		if lower == "none" || lower == "inherit" {
			return true
		}
		toks, ok := splitCSSTokens(lower)
		if !ok || len(toks) == 0 || len(toks) > 3 {
			return false
		}
		for _, tok := range toks {
			if !isSafeBorderWidthToken(tok) && !borderStyleValues[tok] && !isSafeCSSColor(tok) {
				return false
			}
		}
		return true
	case "text-decoration":
		toks, ok := splitCSSTokens(lower)
		if !ok || len(toks) == 0 || len(toks) > 4 {
			return false
		}
		for _, tok := range toks {
			if !textDecorationKeywords[tok] && !isSafeCSSColor(tok) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func isSafeBorderWidthToken(tok string) bool {
	if borderWidthKeywords[tok] {
		return true
	}
	return isSafeCSSLength(tok, true, 0, 50, 0, 40, 0, 5, -1, -1)
}

func isSafeSpacingList(lower string, maxTokens int, allowAuto bool) bool {
	toks, ok := splitCSSTokens(lower)
	if !ok || len(toks) == 0 || len(toks) > maxTokens {
		return false
	}
	for _, tok := range toks {
		if tok == "inherit" || (allowAuto && tok == "auto") {
			continue
		}
		if !isSafeCSSLength(tok, true, 0, 500, 0, 400, 0, 50, 0, 100) {
			return false
		}
	}
	return true
}

func isSafeCSSLength(tok string, allowZero bool, minPx, maxPx, minPt, maxPt, minEm, maxEm, minPct, maxPct float64) bool {
	if tok == "0" {
		return allowZero
	}
	type unitBound struct {
		suffix string
		min    float64
		max    float64
	}
	bounds := []unitBound{
		{"px", minPx, maxPx},
		{"pt", minPt, maxPt},
		{"rem", minEm, maxEm},
		{"em", minEm, maxEm},
		{"%", minPct, maxPct},
	}
	for _, b := range bounds {
		if b.max < 0 {
			continue
		}
		if strings.HasSuffix(tok, b.suffix) {
			numStr := strings.TrimSuffix(tok, b.suffix)
			v, ok := parseUnsignedDecimal(numStr)
			if !ok {
				return false
			}
			return v >= b.min && v <= b.max
		}
	}
	return false
}

func isSafeCSSColor(lower string) bool {
	if strings.HasPrefix(lower, "#") {
		hexPart := lower[1:]
		if len(hexPart) != 3 && len(hexPart) != 4 && len(hexPart) != 6 && len(hexPart) != 8 {
			return false
		}
		for i := 0; i < len(hexPart); i++ {
			c := hexPart[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
		return true
	}

	for _, fn := range []string{"rgb(", "rgba(", "hsl(", "hsla("} {
		if strings.HasPrefix(lower, fn) && strings.HasSuffix(lower, ")") {
			if len(lower) > 80 {
				return false
			}
			inner := lower[len(fn) : len(lower)-1]
			return isSafeColorFuncArgs(inner)
		}
	}

	if len(lower) < 3 || len(lower) > 30 {
		return false
	}
	for i := 0; i < len(lower); i++ {
		if lower[i] < 'a' || lower[i] > 'z' {
			return false
		}
	}
	return true
}

func isSafeColorFuncArgs(inner string) bool {
	if strings.TrimSpace(inner) == "" {
		return false
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '%' || c == ',' || c == '/' || c == ' ' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			continue
		}
		return false
	}
	// Allow only angle unit words (deg, rad, grad, turn) alongside numeric arguments.
	parts := strings.FieldsFunc(inner, func(r rune) bool {
		return r == ',' || r == '/' || r == ' '
	})
	if len(parts) < 3 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		numStr := p
		for _, suffix := range []string{"%", "deg", "rad", "grad", "turn"} {
			if strings.HasSuffix(numStr, suffix) {
				numStr = strings.TrimSuffix(numStr, suffix)
				break
			}
		}
		if _, ok := parseUnsignedDecimal(numStr); !ok {
			return false
		}
	}
	return true
}

func isSafeFontFamily(raw string) bool {
	if len(raw) == 0 || len(raw) > maxFontFamilyLn {
		return false
	}
	families := strings.Split(raw, ",")
	for _, fam := range families {
		fam = strings.TrimSpace(fam)
		if fam == "" {
			return false
		}
		if (strings.HasPrefix(fam, "'") && strings.HasSuffix(fam, "'")) ||
			(strings.HasPrefix(fam, `"`) && strings.HasSuffix(fam, `"`)) {
			if len(fam) < 3 {
				return false
			}
			fam = fam[1 : len(fam)-1]
		}
		for _, r := range fam {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == ' ' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

// splitCSSTokens splits a CSS value on ASCII spaces while keeping parenthesized calls intact.
func splitCSSTokens(s string) ([]string, bool) {
	var tokens []string
	depth := 0
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '(':
			depth++
			if depth > 1 {
				return nil, false
			}
			if start == -1 {
				start = i
			}
		case ')':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ' ':
			if depth == 0 && start != -1 {
				tokens = append(tokens, s[start:i])
				start = -1
			}
		default:
			if start == -1 {
				start = i
			}
		}
	}
	if depth != 0 {
		return nil, false
	}
	if start != -1 {
		tokens = append(tokens, s[start:])
	}
	return tokens, true
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
	if hasUnsafeURLChars(raw) {
		return false
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
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
	case "http", "https":
		return u.Host != ""
	case "mailto":
		return u.Opaque != ""
	default:
		return false
	}
}

func isSafeImgSrc(raw string) bool {
	if hasUnsafeURLChars(raw) {
		return false
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
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
			return isValidBase64Payload(trimmed[len(prefix):])
		}
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return false
	}
	return isPublicImageHost(u.Hostname())
}

func isValidBase64Payload(payload string) bool {
	if payload == "" {
		return false
	}
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		isAlphaNum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '+' && c != '/' && c != '=' {
			return false
		}
	}
	_, err := base64.StdEncoding.DecodeString(payload)
	return err == nil
}

// isPublicImageHost rejects IP literals and local or internal hostnames to limit SSRF in Chromium.
func isPublicImageHost(rawHost string) bool {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rawHost), "."))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	if !strings.Contains(host, ".") {
		return false
	}

	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}

	// Reject numeric or obfuscated IPv4 shorthands such as 0177.0.0.1 by requiring a valid TLD.
	tld := labels[len(labels)-1]
	if strings.HasPrefix(tld, "xn--") {
		return len(tld) > 4
	}
	if len(tld) < 2 {
		return false
	}
	for i := 0; i < len(tld); i++ {
		if tld[i] < 'a' || tld[i] > 'z' {
			return false
		}
	}
	return true
}
