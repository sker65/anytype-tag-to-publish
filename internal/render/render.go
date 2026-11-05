// Package render turns an AnyBlock v2 document into one self-contained HTML
// page. Only the object itself is rendered: dataviews (collections, queries)
// become a placeholder and linked objects are never published along with it.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/sker65/anypub/internal/anyapi"
)

// Version changes whenever output for the same input changes, so stored
// pages are rebuilt after an upgrade.
const Version = 1

// LinkResolver maps an object id to the public URL of that object, if it is published.
type LinkResolver interface {
	ObjectURL(objectID string) (href, title string, ok bool)
}

// AssetSink stores the content of a file object and returns the URL path it
// is served under.
type AssetSink interface {
	Asset(fileID, name, mime string) (href string, err error)
}

type Options struct {
	NoIndex bool
	Links   LinkResolver
	Assets  AssetSink
	// BestEffort renders a placeholder for an asset that fails instead of
	// returning the error.
	BestEffort bool
}

type Result struct {
	HTML     []byte
	Title    string
	Warnings []string
	// Referenced lists object ids the page links to.
	Referenced []string
}

// AssetError marks a non-retryable asset problem that still yields a page.
type AssetError struct{ Msg string }

func (e *AssetError) Error() string { return e.Msg }

type node struct {
	b        anyapi.Block
	children []*node
}

func buildTree(blocks []anyapi.Block) []*node {
	root := &node{}
	type frame struct {
		n      *node
		indent int
	}
	stack := []frame{{root, -1}}
	for _, b := range blocks {
		k := b.Indent
		if k < 0 {
			k = 0
		}
		// lenient: clamp over-deep indents to parent+1
		if top := stack[len(stack)-1].indent; k > top+1 {
			k = top + 1
		}
		for len(stack) > 1 && stack[len(stack)-1].indent >= k {
			stack = stack[:len(stack)-1]
		}
		n := &node{b: b}
		parent := stack[len(stack)-1].n
		parent.children = append(parent.children, n)
		stack = append(stack, frame{n, k})
	}
	return root.children
}

type renderer struct {
	opts     Options
	in       *inline
	warnings []string
	hasCode  bool
	err      error
}

func Render(doc *anyapi.Document, opts Options) (*Result, error) {
	r := &renderer{
		opts: opts,
		in:   &inline{links: opts.Links, referenced: map[string]bool{}},
	}
	var body strings.Builder
	r.nodes(&body, buildTree(doc.Blocks))
	if r.err != nil {
		return nil, r.err
	}

	title := strings.TrimSpace(doc.Name())
	if title == "" {
		title = "Ohne Titel"
	}
	data := pageData{
		Title:       title,
		Description: doc.Description(),
		NoIndex:     opts.NoIndex,
		Body:        template.HTML(body.String()),
		CSS:         template.CSS(baseCSS),
	}
	if doc.Icon != nil && doc.Icon.Format == "emoji" {
		data.Icon = doc.Icon.Emoji
	}
	if r.hasCode {
		data.CodeCSS = template.CSS(codeCSS())
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	res := &Result{HTML: buf.Bytes(), Title: title, Warnings: r.warnings}
	for id := range r.in.referenced {
		res.Referenced = append(res.Referenced, id)
	}
	return res, nil
}

func (r *renderer) warn(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

func (r *renderer) nodes(out *strings.Builder, ns []*node) {
	for i := 0; i < len(ns); {
		t := ns[i].b.Type
		var tag, cls string
		switch t {
		case "bulleted_list_item":
			tag = "ul"
		case "numbered_list_item":
			tag = "ol"
		case "checkbox":
			tag, cls = "ul", ` class="tasks"`
		}
		if tag == "" {
			r.node(out, ns[i])
			i++
			continue
		}
		out.WriteString("<" + tag + cls + ">\n")
		for i < len(ns) && ns[i].b.Type == t {
			r.node(out, ns[i])
			i++
		}
		out.WriteString("</" + tag + ">\n")
	}
}

func classes(b anyapi.Block, extra ...string) string {
	cls := append([]string{}, extra...)
	if c := colorName(b.Color); c != "" {
		cls = append(cls, "c-"+c)
	}
	if c := colorName(b.BackgroundColor); c != "" {
		cls = append(cls, "bg-"+c)
	}
	switch b.Align {
	case "center", "right", "justify":
		cls = append(cls, "al-"+b.Align)
	}
	if len(cls) == 0 {
		return ""
	}
	return ` class="` + strings.Join(cls, " ") + `"`
}

func (r *renderer) children(out *strings.Builder, n *node) {
	if len(n.children) == 0 {
		return
	}
	out.WriteString(`<div class="nested">` + "\n")
	r.nodes(out, n.children)
	out.WriteString("</div>\n")
}

func (r *renderer) text(b anyapi.Block) string { return r.in.render(b.Text) }

func placeholder(msg string) string {
	return `<div class="placeholder">` + html.EscapeString(msg) + "</div>\n"
}

func (r *renderer) node(out *strings.Builder, n *node) {
	b := n.b
	switch b.Type {
	case "paragraph":
		if b.Text == "" && len(n.children) == 0 {
			return
		}
		out.WriteString("<p" + classes(b) + ">" + r.text(b) + "</p>\n")
		r.children(out, n)
	case "heading_1", "heading_2", "heading_3", "heading_4", "header_4":
		lvl := headingLevel(b.Type)
		fmt.Fprintf(out, "<h%d%s>%s</h%d>\n", lvl, classes(b), r.text(b), lvl)
		r.children(out, n)
	case "toggle_heading_1", "toggle_heading_2", "toggle_heading_3":
		lvl := headingLevel(strings.TrimPrefix(b.Type, "toggle_"))
		fmt.Fprintf(out, "<details%s><summary><h%d>%s</h%d></summary>\n", classes(b), lvl, r.text(b), lvl)
		r.nodes(out, n.children)
		out.WriteString("</details>\n")
	case "quote":
		out.WriteString("<blockquote" + classes(b) + "><p>" + r.text(b) + "</p>\n")
		r.nodes(out, n.children)
		out.WriteString("</blockquote>\n")
	case "code":
		r.code(out, b)
		r.children(out, n)
	case "bulleted_list_item", "numbered_list_item":
		out.WriteString("<li" + classes(b) + ">" + r.text(b))
		if len(n.children) > 0 {
			out.WriteString("\n")
			r.nodes(out, n.children)
		}
		out.WriteString("</li>\n")
	case "checkbox":
		checked := ""
		if b.Checked {
			checked = " checked"
		}
		out.WriteString(`<li` + classes(b, "task") + `><input type="checkbox" disabled` + checked + `> <span>` + r.text(b) + "</span>")
		if len(n.children) > 0 {
			out.WriteString("\n")
			r.nodes(out, n.children)
		}
		out.WriteString("</li>\n")
	case "toggle":
		out.WriteString("<details" + classes(b) + "><summary>" + r.text(b) + "</summary>\n")
		r.nodes(out, n.children)
		out.WriteString("</details>\n")
	case "callout":
		out.WriteString("<aside" + classes(b, "callout") + ">")
		if b.Icon != nil && b.Icon.Emoji != "" {
			out.WriteString(`<span class="callout-icon">` + html.EscapeString(b.Icon.Emoji) + "</span>")
		}
		out.WriteString("<div><p>" + r.text(b) + "</p>\n")
		r.nodes(out, n.children)
		out.WriteString("</div></aside>\n")
	case "divider":
		out.WriteString("<hr>\n")
	case "file", "image", "video", "audio", "pdf":
		r.file(out, b)
		r.children(out, n)
	case "bookmark":
		if safeURL(b.URL) {
			label := b.Name
			if label == "" {
				label = b.URL
			}
			out.WriteString(`<p class="bookmark"><a href="` + html.EscapeString(b.URL) + `" rel="noopener noreferrer nofollow">` + html.EscapeString(label) + "</a></p>\n")
		}
	case "link":
		r.in.referenced[b.ObjectID] = true
		if r.opts.Links == nil {
			return
		}
		// unpublished targets are omitted: their name is private
		if href, title, ok := r.opts.Links.ObjectURL(b.ObjectID); ok {
			if title == "" {
				title = href
			}
			out.WriteString(`<p class="objlink"><a href="` + html.EscapeString(href) + `">` + html.EscapeString(title) + "</a></p>\n")
		}
	case "row":
		out.WriteString(`<div class="cols">` + "\n")
		r.nodes(out, n.children)
		out.WriteString("</div>\n")
	case "column":
		out.WriteString(`<div class="col">` + "\n")
		r.nodes(out, n.children)
		out.WriteString("</div>\n")
	case "group":
		r.nodes(out, n.children)
	case "table":
		r.table(out, b)
	case "embed", "equation", "latex":
		r.embed(out, b)
	case "dataview":
		out.WriteString(placeholder("Datenbank-Ansicht (wird nicht veröffentlicht)"))
	case "title", "description", "featured_properties", "property", "table_of_contents", "widget", "chat", "icon":
		// structural or app-only: nothing to show
	default:
		out.WriteString(placeholder("Nicht unterstützter Inhalt (" + b.Type + ")"))
		r.nodes(out, n.children)
	}
}

func headingLevel(t string) int {
	// the page title is h1, so document headings start at h2
	switch t {
	case "heading_1":
		return 2
	case "heading_2":
		return 3
	default:
		return 4
	}
}

var serviceEmbeds = map[string]bool{
	"youtube": true, "vimeo": true, "soundcloud": true, "google_maps": true, "miro": true,
	"figma": true, "twitter": true, "open_street_map": true, "reddit": true, "facebook": true,
	"instagram": true, "telegram": true, "github_gist": true, "codepen": true, "bilibili": true,
	"sketchfab": true, "image": true, "spotify": true,
}

func (r *renderer) embed(out *strings.Builder, b anyapi.Block) {
	p := b.Processor
	if p == "" {
		p = "latex"
	}
	src := strings.TrimSpace(b.Text)
	if src == "" {
		src = strings.TrimSpace(b.URL)
	}
	if serviceEmbeds[p] && safeURL(src) {
		out.WriteString(`<p class="embed-link"><a href="` + html.EscapeString(src) + `" rel="noopener noreferrer nofollow">` + html.EscapeString(src) + "</a></p>\n")
		return
	}
	if p == "latex" && src != "" {
		out.WriteString(`<pre class="math">` + html.EscapeString(src) + "</pre>\n")
		return
	}
	out.WriteString(placeholder("Eingebetteter Inhalt (" + p + ") wird nicht unterstützt"))
}

func (r *renderer) code(out *strings.Builder, b anyapi.Block) {
	lexer := lexers.Get(b.Language)
	if lexer == nil {
		lexer = lexers.Analyse(b.Text)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	it, err := lexer.Tokenise(nil, b.Text)
	if err == nil {
		var buf bytes.Buffer
		if err = codeFormatter.Format(&buf, styles.Get("github"), it); err == nil {
			r.hasCode = true
			out.Write(buf.Bytes())
			out.WriteString("\n")
			return
		}
	}
	out.WriteString("<pre><code>" + html.EscapeString(b.Text) + "</code></pre>\n")
}

var codeFormatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.TabWidth(4))

func codeCSS() string {
	var light, dark bytes.Buffer
	_ = codeFormatter.WriteCSS(&light, styles.Get("github"))
	_ = codeFormatter.WriteCSS(&dark, styles.Get("github-dark"))
	return light.String() + "\n@media (prefers-color-scheme: dark) {\n" + dark.String() + "}\n"
}

func (r *renderer) file(out *strings.Builder, b anyapi.Block) {
	id := b.FileID()
	if id == "" {
		return
	}
	name := b.Name
	if name == "" {
		name = "Datei"
	}
	if r.opts.Assets == nil {
		out.WriteString(placeholder(name))
		return
	}
	href, err := r.opts.Assets.Asset(id, name, b.MimeType)
	if err != nil {
		if ae, ok := err.(*AssetError); ok || r.opts.BestEffort {
			msg := err.Error()
			if ok {
				msg = ae.Msg
			}
			r.warn("%s: %s", name, msg)
			out.WriteString(placeholder(name + " – nicht verfügbar"))
			return
		}
		if r.err == nil {
			r.err = fmt.Errorf("asset %s: %w", id, err)
		}
		return
	}
	h := html.EscapeString(href)
	alt := html.EscapeString(name)
	switch b.Type {
	case "image":
		out.WriteString(`<figure><img src="` + h + `" alt="` + alt + `" loading="lazy"></figure>` + "\n")
	case "video":
		out.WriteString(`<figure><video src="` + h + `" controls preload="metadata"></video></figure>` + "\n")
	case "audio":
		out.WriteString(`<figure><audio src="` + h + `" controls preload="metadata"></audio></figure>` + "\n")
	default:
		out.WriteString(`<p class="file"><a href="` + h + `" download="` + alt + `">` + alt + "</a></p>\n")
	}
}

func (r *renderer) table(out *strings.Builder, b anyapi.Block) {
	out.WriteString(`<div class="table-wrap"><table>` + "\n")
	for _, row := range b.Rows {
		cell := "td"
		if row.IsHeader {
			cell = "th"
		}
		out.WriteString("<tr>")
		for i := range b.Columns {
			out.WriteString("<" + cell + ">")
			if i < len(row.Cells) {
				r.cell(out, row.Cells[i])
			}
			out.WriteString("</" + cell + ">")
		}
		out.WriteString("</tr>\n")
	}
	out.WriteString("</table></div>\n")
}

func (r *renderer) cell(out *strings.Builder, raw json.RawMessage) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out.WriteString(r.in.render(s))
		}
	case '{':
		var b anyapi.Block
		if json.Unmarshal(raw, &b) == nil {
			r.cellBlocks(out, []anyapi.Block{b})
		}
	case '[':
		var bs []anyapi.Block
		if json.Unmarshal(raw, &bs) == nil {
			r.cellBlocks(out, bs)
		}
	}
}

func (r *renderer) cellBlocks(out *strings.Builder, bs []anyapi.Block) {
	ns := buildTree(bs)
	// a single plain paragraph renders without the <p> wrapper
	if len(ns) == 1 && ns[0].b.Type == "paragraph" && len(ns[0].children) == 0 {
		out.WriteString(`<span` + classes(ns[0].b) + `>` + r.text(ns[0].b) + "</span>")
		return
	}
	r.nodes(out, ns)
}
