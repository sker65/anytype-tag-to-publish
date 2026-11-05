package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sker65/anypub/internal/anyapi"
	"github.com/sker65/anypub/internal/config"
)

type harness struct {
	t      *testing.T
	fake   *fakeAnytype
	cfg    config.Config
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T) *harness {
	f := newFake()
	t.Cleanup(f.Close)
	f.addSpace("space1")
	f.addSpace("space2")

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Anytype.APIAddr = f.srv.URL
	cfg.Anytype.APIKey = "test-key"
	cfg.Anytype.Heartbeat = time.Second
	cfg.Spaces = []config.Space{{ID: "space1"}} // space2 is NOT configured
	cfg.Publish.BaseURL = "https://pub.example.com"
	cfg.Publish.OutDir = filepath.Join(dir, "public")
	cfg.Publish.Debounce = 150 * time.Millisecond
	cfg.Publish.SafetyReconcile = 0
	cfg.State.Path = filepath.Join(dir, "state.db")
	cfg.Obs.ListenAddr = "127.0.0.1:0"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, fake: f, cfg: cfg}
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	go func() { h.done <- Run(ctx, h.cfg, log) }()
}

func (h *harness) stop() {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Fatalf("service: %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("service did not stop")
	}
}

func (h *harness) waitFor(what string, cond func() bool) time.Duration {
	h.t.Helper()
	start := time.Now()
	for time.Since(start) < 5*time.Second {
		if cond() {
			return time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
	return 0
}

func (h *harness) url(id string) string {
	v, _ := h.fake.prop(id, "public_url").(string)
	return v
}

func (h *harness) page(url string) (string, bool) {
	slug := strings.TrimPrefix(url, h.cfg.Publish.BaseURL+"/p/")
	b, err := os.ReadFile(filepath.Join(h.cfg.Publish.OutDir, "p", slug, "index.html"))
	return string(b), err == nil
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	f := h.fake
	f.mu.Lock()
	f.files["img1"] = []byte("\x89PNG fake image")
	f.mu.Unlock()

	f.put(&fakeObj{ID: "A", Space: "space1", Name: "Alpha", Tags: []string{"publish"}, Blocks: []anyapi.Block{
		{Type: "paragraph", Text: "Hello **world**"},
		{Type: "image", ObjectID: "img1", Name: "pic.png"},
	}})
	f.put(&fakeObj{ID: "B", Space: "space1", Name: "Beta", Blocks: []anyapi.Block{
		{Type: "paragraph", Text: `See <mention object_id="A">Alpha</mention>`},
	}})
	f.put(&fakeObj{ID: "C", Space: "space2", Name: "Other space", Tags: []string{"publish"}})

	h.start()

	// publish from the initial snapshot
	h.waitFor("A published", func() bool { return h.url("A") != "" })
	urlA := h.url("A")
	if !strings.HasPrefix(urlA, "https://pub.example.com/p/") || len(strings.TrimPrefix(urlA, "https://pub.example.com/p/")) != 22 {
		t.Fatalf("unexpected URL %q", urlA)
	}
	html, ok := h.page(urlA)
	if !ok || !strings.Contains(html, "Hello <strong>world</strong>") {
		t.Fatalf("page A missing or wrong:\n%s", html)
	}
	if !strings.Contains(html, `/a/`) || !strings.Contains(html, `.png"`) {
		t.Fatalf("image not rewritten:\n%s", html)
	}
	if f.prop("A", "published_at") == nil {
		t.Error("published_at not written")
	}

	// unconfigured space is ignored even though tagged
	if f.prop("C", "public_url") != nil {
		t.Error("object in unconfigured space was published")
	}

	// the write-back must not cause another render
	time.Sleep(4 * h.cfg.Publish.Debounce)
	if n := f.getCount("A"); n != 1 {
		t.Errorf("A read %d times, want 1 (write-back caused a re-render)", n)
	}

	// tag set later: published by push, under 5 s
	f.mutate("B", func(o *fakeObj) { o.Tags = []string{"publish"} })
	lat := h.waitFor("B published", func() bool { return h.url("B") != "" })
	t.Logf("tag -> URL latency: %v", lat)
	htmlB, _ := h.page(h.url("B"))
	if !strings.Contains(htmlB, `<a href="`+urlA+`">Alpha</a>`) {
		t.Errorf("mention of published object not rewritten:\n%s", htmlB)
	}

	// content edit: page updated after debounce
	f.mutate("A", func(o *fakeObj) { o.Blocks[0].Text = "Hello again" })
	h.waitFor("A updated", func() bool { p, _ := h.page(urlA); return strings.Contains(p, "Hello again") })
	time.Sleep(4 * h.cfg.Publish.Debounce)
	if n := f.getCount("A"); n != 2 {
		t.Errorf("A read %d times after one edit, want 2", n)
	}

	// untag: page gone, URL cleared
	f.mutate("A", func(o *fakeObj) { o.Tags = nil })
	h.waitFor("A unpublished", func() bool { _, ok := h.page(urlA); return !ok && h.url("A") == "" })

	// a lost event while the stream is down is caught by the reconnect snapshot
	urlB := h.url("B")
	f.DropStreams()
	f.mutate("B", func(o *fakeObj) { o.Tags = nil })
	h.waitFor("B unpublished after reconnect", func() bool { _, ok := h.page(urlB); return !ok })

	// only bot properties were ever written
	for _, p := range f.patchedKeys() {
		key := p[strings.Index(p, ":")+1:]
		if key != "public_url" && key != "published_at" && key != "publish_error" {
			t.Errorf("wrote non-bot property %s", p)
		}
	}
	h.stop()
}

func TestStateLossKeepsSlug(t *testing.T) {
	h := newHarness(t)
	h.fake.put(&fakeObj{ID: "A", Space: "space1", Name: "Alpha", Tags: []string{"publish"}, Blocks: []anyapi.Block{{Type: "paragraph", Text: "x"}}})
	h.start()
	h.waitFor("A published", func() bool { return h.url("A") != "" })
	url := h.url("A")
	h.stop()

	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(h.cfg.State.Path + suffix)
	}

	h.start()
	h.waitFor("A re-rendered", func() bool { return h.fake.getCount("A") >= 2 })
	h.waitFor("page served", func() bool { _, ok := h.page(url); return ok })
	if got := h.url("A"); got != url {
		t.Fatalf("URL changed after state loss: %q -> %q", url, got)
	}
	h.stop()
}

func TestRestartWithoutChangesDoesNotRender(t *testing.T) {
	h := newHarness(t)
	h.fake.put(&fakeObj{ID: "A", Space: "space1", Name: "Alpha", Tags: []string{"publish"}})
	h.start()
	h.waitFor("A published", func() bool { return h.url("A") != "" })
	time.Sleep(4 * h.cfg.Publish.Debounce)
	h.stop()

	h.start()
	time.Sleep(500 * time.Millisecond)
	if n := h.fake.getCount("A"); n != 1 {
		t.Errorf("A read %d times across a restart without changes, want 1", n)
	}
	h.stop()
}

// In a fresh space the tag option does not exist, and the v2 API refuses a
// filter that names it. anypub must wait for it instead of failing.
func TestTagOptionCreatedLater(t *testing.T) {
	h := newHarness(t)
	h.start()
	defer h.stop()
	time.Sleep(300 * time.Millisecond) // first stream attempt is refused
	h.fake.put(&fakeObj{ID: "late", Space: "space1", Name: "Spät", Tags: []string{"publish"},
		Blocks: []anyapi.Block{{Type: "text", Text: "hallo", Style: "paragraph"}}})
	h.waitFor("publish after the option appeared", func() bool { return h.url("late") != "" })
}

// When the tag option disappears while the stream is down, nothing carries the
// tag any more: the reconnect must unpublish, although the filter is refused.
func TestTagOptionRemovedUnpublishes(t *testing.T) {
	h := newHarness(t)
	h.fake.put(&fakeObj{ID: "o", Space: "space1", Name: "Weg", Tags: []string{"publish"},
		Blocks: []anyapi.Block{{Type: "text", Text: "hallo", Style: "paragraph"}}})
	h.start()
	defer h.stop()
	h.waitFor("publish", func() bool { return h.url("o") != "" })
	url := h.url("o")

	h.fake.mu.Lock()
	h.fake.objects["o"].Tags = nil
	delete(h.fake.options["space1"], "publish")
	h.fake.mu.Unlock()
	h.fake.DropStreams()

	h.waitFor("unpublish", func() bool { _, ok := h.page(url); return !ok })
}
