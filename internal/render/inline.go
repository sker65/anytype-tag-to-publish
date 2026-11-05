package render

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// inline renders AnyBlock inline markup (any-block format/v2 INLINE_MARKUP.md)
// to HTML. It is a renderer, not a validator: malformed markup degrades to
// literal text instead of failing.
//
// Grammar: **bold**, *italic* (_italic_), ~~strike~~, `code`, [t](url),
// [t](anytype://object?objectId=<id>), <mention object_id="…">t</mention>,
// <u>t</u>, <font color|background="…">t</font>, backslash escapes, \n.
type inline struct {
	links LinkResolver
	// referenced collects object ids the text links to.
	referenced map[string]bool
}

func (in *inline) render(s string) string {
	var out strings.Builder
	in.parse(s, &out, 0)
	return out.String()
}

const maxDepth = 32

func (in *inline) parse(s string, out *strings.Builder, depth int) {
	if depth > maxDepth {
		out.WriteString(html.EscapeString(s))
		return
	}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out.WriteString(html.EscapeString(html.UnescapeString(lit.String())))
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) && isASCIIPunct(s[i+1]) {
				flush()
				out.WriteString(html.EscapeString(s[i+1 : i+2]))
				i += 2
				continue
			}
		case '\n':
			flush()
			out.WriteString("<br>\n")
			i++
			continue
		case '`':
			if end, content, ok := codeSpan(s, i); ok {
				flush()
				out.WriteString("<code>")
				out.WriteString(html.EscapeString(content))
				out.WriteString("</code>")
				i = end
				continue
			}
			// literal backtick run
			n := runLen(s, i, '`')
			lit.WriteString(s[i : i+n])
			i += n
			continue
		case '<':
			if end, ok := in.tag(s, i, out, depth, flush); ok {
				i = end
				continue
			}
		case '[':
			if end, ok := in.link(s, i, out, depth, flush); ok {
				i = end
				continue
			}
		case '*', '_':
			if end, ok := in.emphasis(s, i, out, depth, flush); ok {
				i = end
				continue
			}
			n := runLen(s, i, c)
			lit.WriteString(s[i : i+n])
			i += n
			continue
		case '~':
			if strings.HasPrefix(s[i:], "~~") && runLen(s, i, '~') == 2 {
				if j := findCloser(s, i+2, "~~"); j > i+2 && !isSpace(s[i+2]) && !isSpace(s[j-1]) {
					flush()
					out.WriteString("<del>")
					in.parse(s[i+2:j], out, depth+1)
					out.WriteString("</del>")
					i = j + 2
					continue
				}
			}
			n := runLen(s, i, '~')
			lit.WriteString(s[i : i+n])
			i += n
			continue
		}
		lit.WriteByte(c)
		i++
	}
	flush()
}

func (in *inline) emphasis(s string, i int, out *strings.Builder, depth int, flush func()) (int, bool) {
	c := s[i]
	run := runLen(s, i, c)
	if c == '_' {
		// intraword underscores stay literal
		if i > 0 && isAlnum(s[i-1]) {
			return 0, false
		}
	}
	try := func(n int, open, close string) (int, bool) {
		d := strings.Repeat(string(c), n)
		start := i + n
		if start >= len(s) || isSpace(s[start]) {
			return 0, false
		}
		j := findCloser(s, start, d)
		if j <= start || isSpace(s[j-1]) {
			return 0, false
		}
		if c == '_' && j+n < len(s) && isAlnum(s[j+n]) {
			return 0, false
		}
		flush()
		out.WriteString(open)
		in.parse(s[start:j], out, depth+1)
		out.WriteString(close)
		return j + n, true
	}
	if run >= 2 {
		if end, ok := try(2, "<strong>", "</strong>"); ok {
			return end, true
		}
	}
	if run == 1 || run >= 3 {
		return try(1, "<em>", "</em>")
	}
	return 0, false
}

// findCloser finds delim starting at from, skipping escapes and code spans.
// A single-character delimiter does not match inside a double run.
func findCloser(s string, from int, delim string) int {
	for j := from; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
			continue
		case '`':
			if end, _, ok := codeSpan(s, j); ok {
				j = end - 1
				continue
			}
		}
		if !strings.HasPrefix(s[j:], delim) {
			continue
		}
		run := runLen(s, j, delim[0])
		if len(delim) == 1 && run == 2 {
			j++ // skip the double run
			continue
		}
		if len(delim) == 2 && run == 1 {
			continue
		}
		return j
	}
	return -1
}

func codeSpan(s string, i int) (end int, content string, ok bool) {
	n := runLen(s, i, '`')
	for j := i + n; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		m := runLen(s, j, '`')
		if m == n {
			c := s[i+n : j]
			if len(c) >= 2 && c[0] == ' ' && c[len(c)-1] == ' ' && strings.Trim(c, " ") != "" {
				c = c[1 : len(c)-1]
			}
			return j + m, c, true
		}
		j += m
	}
	return 0, "", false
}

var attrRe = regexp.MustCompile(`([a-z_]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// tag handles <u>, <font …> and <mention object_id="…">.
func (in *inline) tag(s string, i int, out *strings.Builder, depth int, flush func()) (int, bool) {
	rest := s[i:]
	var name string
	switch {
	case strings.HasPrefix(rest, "<u>"):
		name = "u"
	case strings.HasPrefix(rest, "<font ") || strings.HasPrefix(rest, "<font>"):
		name = "font"
	case strings.HasPrefix(rest, "<mention ") || strings.HasPrefix(rest, "<mention>"):
		name = "mention"
	default:
		return 0, false
	}
	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return 0, false
	}
	attrs := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(rest[len(name)+1:gt], -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		attrs[m[1]] = html.UnescapeString(v)
	}
	innerStart := i + gt + 1
	closeIdx := matchClose(s, innerStart, name)
	if closeIdx < 0 {
		return 0, false
	}
	inner := s[innerStart:closeIdx]
	end := closeIdx + len("</"+name+">")
	flush()
	switch name {
	case "u":
		out.WriteString("<u>")
		in.parse(inner, out, depth+1)
		out.WriteString("</u>")
	case "font":
		var cls []string
		if c := colorName(attrs["color"]); c != "" {
			cls = append(cls, "c-"+c)
		}
		if c := colorName(attrs["background"]); c != "" {
			cls = append(cls, "bg-"+c)
		}
		if len(cls) == 0 {
			in.parse(inner, out, depth+1)
			break
		}
		out.WriteString(`<span class="` + strings.Join(cls, " ") + `">`)
		in.parse(inner, out, depth+1)
		out.WriteString("</span>")
	case "mention":
		in.objectRef(attrs["object_id"], inner, out, depth)
	}
	return end, true
}

func matchClose(s string, from int, name string) int {
	open, close := "<"+name, "</"+name+">"
	level := 0
	for j := from; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if strings.HasPrefix(s[j:], close) {
			if level == 0 {
				return j
			}
			level--
			continue
		}
		if strings.HasPrefix(s[j:], open) && j+len(open) < len(s) && (s[j+len(open)] == '>' || s[j+len(open)] == ' ') {
			level++
		}
	}
	return -1
}

// link handles [label](dest).
func (in *inline) link(s string, i int, out *strings.Builder, depth int, flush func()) (int, bool) {
	// find the matching ]
	level := 0
	j := i
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
			continue
		case '`':
			if end, _, ok := codeSpan(s, j); ok {
				j = end - 1
				continue
			}
		case '[':
			level++
			if level > maxDepth {
				return 0, false
			}
		case ']':
			level--
		}
		if level == 0 {
			break
		}
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return 0, false
	}
	label := s[i+1 : j]
	dest, end, ok := linkDest(s, j+2)
	if !ok {
		return 0, false
	}
	flush()
	if id, ok := objectLinkID(dest); ok {
		in.objectRef(id, label, out, depth)
		return end, true
	}
	if safeURL(dest) {
		out.WriteString(`<a href="` + html.EscapeString(dest) + `" rel="noopener noreferrer nofollow">`)
		in.parse(label, out, depth+1)
		out.WriteString("</a>")
	} else {
		in.parse(label, out, depth+1)
	}
	return end, true
}

// linkDest parses a link destination starting after "(" and returns the
// unescaped URL and the index after ")".
func linkDest(s string, i int) (string, int, bool) {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	var b strings.Builder
	if i < len(s) && s[i] == '<' {
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				if j+1 < len(s) {
					b.WriteByte(s[j+1])
					j++
				}
			case '>':
				k := j + 1
				for k < len(s) && isSpace(s[k]) {
					k++
				}
				if k < len(s) && s[k] == ')' {
					return b.String(), k + 1, true
				}
				return "", 0, false
			case '\n':
				return "", 0, false
			default:
				b.WriteByte(s[j])
			}
		}
		return "", 0, false
	}
	parens := 0
	for j := i; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '\\' && j+1 < len(s) && isASCIIPunct(s[j+1]):
			b.WriteByte(s[j+1])
			j++
		case c == '(':
			parens++
			b.WriteByte(c)
		case c == ')':
			if parens == 0 {
				return b.String(), j + 1, true
			}
			parens--
			b.WriteByte(c)
		case isSpace(c):
			k := j
			for k < len(s) && isSpace(s[k]) {
				k++
			}
			if k < len(s) && s[k] == ')' && parens == 0 {
				return b.String(), k + 1, true
			}
			return "", 0, false
		default:
			b.WriteByte(c)
		}
		if b.Len() > 8192 {
			return "", 0, false
		}
	}
	return "", 0, false
}

// objectLinkID accepts anytype://object?objectId=<id> (optionally with spaceId).
func objectLinkID(dest string) (string, bool) {
	if !strings.HasPrefix(dest, "anytype://object?") {
		return "", false
	}
	u, err := url.Parse(dest)
	if err != nil {
		return "", false
	}
	id := u.Query().Get("objectId")
	return id, id != ""
}

func (in *inline) objectRef(objectID, label string, out *strings.Builder, depth int) {
	if objectID != "" && in.referenced != nil {
		in.referenced[objectID] = true
	}
	if objectID != "" && in.links != nil {
		if href, _, ok := in.links.ObjectURL(objectID); ok {
			out.WriteString(`<a href="` + html.EscapeString(href) + `">`)
			in.parse(label, out, depth+1)
			out.WriteString("</a>")
			return
		}
	}
	in.parse(label, out, depth+1)
}

func safeURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	switch strings.ToLower(p.Scheme) {
	case "http", "https", "mailto":
		return true
	}
	return false
}

var palette = map[string]bool{
	"grey": true, "yellow": true, "orange": true, "red": true, "pink": true,
	"purple": true, "blue": true, "ice": true, "teal": true, "lime": true,
	"default": false,
}

func colorName(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if palette[c] {
		return c
	}
	return ""
}

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}
