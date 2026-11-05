package render

import "html/template"

type pageData struct {
	Title       string
	Description string
	Icon        string
	NoIndex     bool
	Body        template.HTML
	CSS         template.CSS
	CodeCSS     template.CSS
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="de">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .NoIndex}}<meta name="robots" content="noindex, nofollow">
{{end}}<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<style>{{.CSS}}</style>
{{if .CodeCSS}}<style>{{.CodeCSS}}</style>
{{end}}</head>
<body>
<main>
<header>
{{if .Icon}}<div class="icon">{{.Icon}}</div>
{{end}}<h1>{{.Title}}</h1>
{{if .Description}}<p class="description">{{.Description}}</p>
{{end}}</header>
<article>
{{.Body}}
</article>
</main>
</body>
</html>
`))

const baseCSS = `
:root {
  --bg: #ffffff; --fg: #1f2328; --muted: #656d76; --border: #d0d7de;
  --surface: #f6f8fa; --link: #0969da;
  --grey: #8c8c8c; --yellow: #c9a200; --orange: #e07a00; --red: #d92d20; --pink: #d6338a;
  --purple: #8250df; --blue: #0969da; --ice: #2a9fd6; --teal: #0f9d8f; --lime: #5aa700;
  --bg-grey: #eeeeee; --bg-yellow: #fdf4bf; --bg-orange: #feead2; --bg-red: #fde3e1; --bg-pink: #fce4f0;
  --bg-purple: #efe6fc; --bg-blue: #ddeeff; --bg-ice: #dcf3fb; --bg-teal: #d7f4ef; --bg-lime: #e6f6d0;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0d1117; --fg: #e6edf3; --muted: #8d96a0; --border: #30363d;
    --surface: #161b22; --link: #4493f8;
    --bg-grey: #2b2b2b; --bg-yellow: #3d3510; --bg-orange: #3f2a12; --bg-red: #44201d; --bg-pink: #43203a;
    --bg-purple: #30244a; --bg-blue: #172c46; --bg-ice: #163842; --bg-teal: #143a35; --bg-lime: #263a14;
  }
}
* { box-sizing: border-box; }
html { background: var(--bg); color: var(--fg); }
body { margin: 0; font: 17px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
main { max-width: 46rem; margin: 0 auto; padding: 3rem 1rem 5rem; }
header .icon { font-size: 3rem; line-height: 1; }
h1 { font-size: 2.2rem; line-height: 1.2; margin: .5rem 0 1rem; }
h2, h3, h4 { line-height: 1.3; margin: 1.8rem 0 .6rem; }
.description { color: var(--muted); font-size: 1.1rem; margin-top: -.5rem; }
a { color: var(--link); }
p { margin: .5rem 0; }
blockquote { margin: 1rem 0; padding: .1rem 1rem; border-left: 3px solid var(--border); }
code { font: .9em ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; background: var(--surface); padding: .1em .3em; border-radius: 4px; }
pre { overflow-x: auto; padding: 1rem; border-radius: 6px; background: var(--surface); font: .9em/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
pre code { background: none; padding: 0; }
hr { border: 0; border-top: 1px solid var(--border); margin: 1.5rem 0; }
ul.tasks { list-style: none; padding-left: .2rem; }
li.task > input { margin-right: .4rem; }
.nested { margin-left: 1.5rem; }
details { margin: .5rem 0; }
details > summary { cursor: pointer; }
details > summary > h2, details > summary > h3, details > summary > h4 { display: inline; }
details > :not(summary) { margin-left: 1.2rem; }
.callout { display: flex; gap: .7rem; padding: .8rem 1rem; margin: 1rem 0; border-radius: 6px; background: var(--surface); }
.callout-icon { font-size: 1.3rem; line-height: 1.4; }
.callout p { margin: 0; }
figure { margin: 1rem 0; }
figure img, figure video { max-width: 100%; height: auto; border-radius: 4px; }
figure audio { width: 100%; }
.cols { display: flex; gap: 1.5rem; flex-wrap: wrap; }
.col { flex: 1 1 14rem; min-width: 0; }
.table-wrap { overflow-x: auto; margin: 1rem 0; }
table { border-collapse: collapse; }
th, td { border: 1px solid var(--border); padding: .4rem .7rem; vertical-align: top; text-align: left; }
th { background: var(--surface); }
.placeholder { color: var(--muted); font-style: italic; border: 1px dashed var(--border); border-radius: 6px; padding: .6rem 1rem; margin: 1rem 0; }
.al-center { text-align: center; } .al-right { text-align: right; } .al-justify { text-align: justify; }
.c-grey { color: var(--grey); } .c-yellow { color: var(--yellow); } .c-orange { color: var(--orange); }
.c-red { color: var(--red); } .c-pink { color: var(--pink); } .c-purple { color: var(--purple); }
.c-blue { color: var(--blue); } .c-ice { color: var(--ice); } .c-teal { color: var(--teal); } .c-lime { color: var(--lime); }
.bg-grey { background: var(--bg-grey); } .bg-yellow { background: var(--bg-yellow); } .bg-orange { background: var(--bg-orange); }
.bg-red { background: var(--bg-red); } .bg-pink { background: var(--bg-pink); } .bg-purple { background: var(--bg-purple); }
.bg-blue { background: var(--bg-blue); } .bg-ice { background: var(--bg-ice); } .bg-teal { background: var(--bg-teal); } .bg-lime { background: var(--bg-lime); }
`
