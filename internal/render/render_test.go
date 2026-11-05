package render

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sker65/anypub/internal/anyapi"
)

type links map[string]string

func (l links) ObjectURL(id string) (string, string, bool) {
	u, ok := l[id]
	return u, "Title of " + id, ok
}

func TestInline(t *testing.T) {
	in := &inline{links: links{"pub": "https://x/p/s1"}, referenced: map[string]bool{}}
	cases := map[string]string{
		"**bold** and *it* and _it2_":               "<strong>bold</strong> and <em>it</em> and <em>it2</em>",
		"a_b_c":                                     "a_b_c",
		"~~gone~~":                                  "<del>gone</del>",
		"`<u>x</u>` and `a**b`":                     "<code>&lt;u&gt;x&lt;/u&gt;</code> and <code>a**b</code>",
		`\*not\* \<u>`:                              "*not* &lt;u&gt;",
		"<u>under</u>":                              "<u>under</u>",
		`<font color="red">r</font>`:                `<span class="c-red">r</span>`,
		`<font color="evil">r</font>`:               "r",
		"[site](https://e.com/a_(b))":               `<a href="https://e.com/a_(b)" rel="noopener noreferrer nofollow">site</a>`,
		"[bad](javascript:alert(1))":                "bad",
		"[x](anytype://object?objectId=pub)":        `<a href="https://x/p/s1">x</a>`,
		"[y](anytype://object?objectId=priv)":       "y",
		`<mention object_id="priv">Alice</mention>`: "Alice",
		"line1\nline2":                              "line1<br>\nline2",
		"<b>raw</b> & &lt;":                         "&lt;b&gt;raw&lt;/b&gt; &amp; &lt;",
		"![img](x.png)":                             "!img",
		"*a **b** c*":                               "<em>a <strong>b</strong> c</em>",
		"unclosed **bold":                           "unclosed **bold",
	}
	for src, want := range cases {
		if got := in.render(src); got != want {
			t.Errorf("render(%q)\n got  %q\n want %q", src, got, want)
		}
	}
	if !in.referenced["pub"] || !in.referenced["priv"] {
		t.Errorf("referenced = %v", in.referenced)
	}
}

type sink struct{ fail error }

func (s sink) Asset(id, name, mime string) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	return "/p/slug/a/" + id + ".png", nil
}

func doc(t *testing.T, js string) *anyapi.Document {
	t.Helper()
	var d anyapi.Document
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

const sample = `{
  "id": "obj1", "type": "page",
  "icon": {"format": "emoji", "emoji": "⛰️"},
  "properties": {"name": "Ridge <first>", "description": "A walk"},
  "blocks": [
    {"id": "b1", "type": "property", "property": "Weather"},
    {"id": "b2", "type": "heading_2", "text": "What happened"},
    {"id": "b3", "type": "paragraph", "text": "A **long** walk to <mention object_id=\"other\">Notebook</mention>."},
    {"id": "b5", "type": "toggle", "text": "Times"},
    {"indent": 1, "id": "b6", "type": "bulleted_list_item", "text": "Left 07:12"},
    {"indent": 1, "id": "b7", "type": "bulleted_list_item", "text": "Back 11:40"},
    {"id": "b8", "type": "checkbox", "checked": true, "text": "Photos copied"},
    {"id": "b9", "type": "image", "object_id": "img1", "name": "ridge.png"},
    {"id": "b10", "type": "code", "language": "go", "text": "fmt.Println(\"<ridge>\")"},
    {"id": "b11", "type": "dataview"},
    {"id": "b12", "type": "link", "object_id": "secret"},
    {"id": "b13", "type": "table", "columns": [{"id": "c1"}, {"id": "c2"}],
     "rows": [{"id": "r1", "is_header": true, "cells": ["Name", "Done"]},
              {"id": "r2", "cells": ["Export", {"type": "checkbox", "checked": true, "text": "yes"}]},
              {"id": "r3", "cells": [null]}]},
    {"id": "b14", "type": "embed", "processor": "mermaid", "text": "graph TD; A-->B"}
  ]
}`

func TestRenderDocument(t *testing.T) {
	res, err := Render(doc(t, sample), Options{NoIndex: true, Links: links{}, Assets: sink{}})
	if err != nil {
		t.Fatal(err)
	}
	h := string(res.HTML)
	for _, want := range []string{
		`<meta name="robots" content="noindex, nofollow">`,
		"<title>Ridge &lt;first&gt;</title>",
		"<h1>Ridge &lt;first&gt;</h1>",
		`<p class="description">A walk</p>`,
		"<h3>What happened</h3>",
		"A <strong>long</strong> walk to Notebook.",
		"<details><summary>Times</summary>\n<ul>\n<li>Left 07:12</li>\n<li>Back 11:40</li>\n</ul>\n</details>",
		`<input type="checkbox" disabled checked>`,
		`<img src="/p/slug/a/img1.png" alt="ridge.png"`,
		`class="chroma"`,
		"Datenbank-Ansicht",
		"<th>Name</th>",
		"Eingebetteter Inhalt (mermaid)",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(h, "secret") {
		t.Error("link to unpublished object leaked")
	}
	if strings.Contains(h, "<script") {
		t.Error("output contains a script")
	}
}

func TestRenderAssetErrors(t *testing.T) {
	d := doc(t, `{"properties":{"name":"x"},"blocks":[{"type":"image","object_id":"i","name":"a.png"}]}`)
	if _, err := Render(d, Options{Assets: sink{fail: errors.New("boom")}}); err == nil {
		t.Fatal("transient asset error should fail the render")
	}
	res, err := Render(d, Options{Assets: sink{fail: errors.New("boom")}, BestEffort: true})
	if err != nil || len(res.Warnings) != 1 {
		t.Fatalf("best effort: %v %v", err, res)
	}
	res, err = Render(d, Options{Assets: sink{fail: &AssetError{Msg: "too large"}}})
	if err != nil || !strings.Contains(string(res.HTML), "nicht verfügbar") {
		t.Fatalf("asset error: %v", err)
	}
}

func TestRenderDeterministic(t *testing.T) {
	a, _ := Render(doc(t, sample), Options{Assets: sink{}})
	b, _ := Render(doc(t, sample), Options{Assets: sink{}})
	if string(a.HTML) != string(b.HTML) {
		t.Fatal("render is not deterministic")
	}
}
