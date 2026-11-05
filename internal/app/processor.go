// Package app holds the publish/update/unpublish logic. It is driven by the
// watcher and knows nothing about streams.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/sker65/anypub/internal/anyapi"
	"github.com/sker65/anypub/internal/obs"
	"github.com/sker65/anypub/internal/publish"
	"github.com/sker65/anypub/internal/render"
	"github.com/sker65/anypub/internal/store"
)

// API is the part of anyapi.Client the processor needs.
type API interface {
	GetObject(ctx context.Context, spaceID, objectID string) (*anyapi.Document, error)
	PatchProperties(ctx context.Context, spaceID, objectID string, set map[string]any, unset []string) error
	DownloadFile(ctx context.Context, spaceID, fileID string, max int64, w io.Writer) (string, error)
}

// PropKeys are the resolved property keys of the bot-owned properties.
// PublishedAt and PublishError are optional ("" = disabled).
type PropKeys struct {
	PublicURL    string
	PublishedAt  string
	PublishError string
}

type Options struct {
	BaseURL             string
	NoIndex             bool
	KeepSlugOnRepublish bool
	MaxAssetBytes       int64
	OnUnpublishHook     string
}

type Processor struct {
	api     API
	store   *store.Store
	out     *publish.Writer
	keys    map[string]PropKeys // per space
	opts    Options
	metrics *obs.Metrics
	log     *slog.Logger
	now     func() time.Time
}

func NewProcessor(api API, st *store.Store, out *publish.Writer, keys map[string]PropKeys, opts Options, m *obs.Metrics, log *slog.Logger) *Processor {
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	return &Processor{api: api, store: st, out: out, keys: keys, opts: opts, metrics: m, log: log, now: time.Now}
}

// Task is one unit of work for an object. Row is the latest stream row.
type Task struct {
	SpaceID  string
	ObjectID string
	Remove   bool
	Row      anyapi.Row
	// OnlySelf is true when every coalesced event was caused by the bot's own write-back.
	OnlySelf bool
	Received time.Time
	// Final is set on the last retry: render with placeholders instead of failing.
	Final bool
}

func (p *Processor) Handle(ctx context.Context, t Task) error {
	if t.Remove {
		return p.Unpublish(ctx, t.SpaceID, t.ObjectID)
	}
	return p.Ensure(ctx, t)
}

func (p *Processor) pageURL(slug string) string { return p.opts.BaseURL + "/p/" + slug }

// ObjectURL implements render.LinkResolver: only published objects resolve.
func (p *Processor) ObjectURL(objectID string) (string, string, bool) {
	e, err := p.store.Get(context.Background(), objectID)
	if err != nil || !e.Active {
		return "", "", false
	}
	return p.pageURL(e.Slug), e.Title, true
}

// Ensure publishes the object or brings its page up to date.
func (p *Processor) Ensure(ctx context.Context, t Task) error {
	row := t.Row
	keys := p.keys[t.SpaceID]
	if keys.PublicURL == "" {
		return fmt.Errorf("space %s is not configured", t.SpaceID)
	}
	modified := row.Prop("last_modified_date")
	currentURL := row.Prop(keys.PublicURL)

	e, err := p.store.Get(ctx, t.ObjectID)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if exists && e.SpaceID != t.SpaceID {
		p.log.Warn("object moved between spaces, ignoring", "object_id", t.ObjectID, "space", t.SpaceID)
		return nil
	}

	upToDate := exists && e.Active && e.RenderVersion == render.Version && p.out.Exists(e.Slug)
	if upToDate && currentURL == p.pageURL(e.Slug) {
		if t.OnlySelf {
			// echo of our own write-back: remember the new revision, do not render
			e.SourceModified = modified
			p.metrics.Renders.WithLabelValues("skipped_self").Inc()
			return p.store.Put(ctx, e)
		}
		if modified != "" && modified == e.SourceModified {
			p.metrics.Renders.WithLabelValues("skipped_unmodified").Inc()
			return nil
		}
	}

	if !exists {
		e = store.Entry{ObjectID: t.ObjectID, SpaceID: t.SpaceID, Mode: "slug"}
		e.Slug = p.recoverSlug(ctx, currentURL)
		if e.Slug == "" {
			if e.Slug, err = p.newSlug(ctx); err != nil {
				return err
			}
		}
	}

	doc, err := p.api.GetObject(ctx, t.SpaceID, t.ObjectID)
	if err != nil {
		if anyapi.IsNotFound(err) {
			p.log.Info("object vanished before rendering", "object_id", t.ObjectID)
			return nil
		}
		return fmt.Errorf("read object: %w", err)
	}

	st, err := p.out.Stage()
	if err != nil {
		return err
	}
	sink := &assetSink{p: p, ctx: ctx, st: st, prev: e, space: t.SpaceID, base: "/p/" + e.Slug + "/", used: map[string]string{}}
	res, err := render.Render(doc, render.Options{NoIndex: p.opts.NoIndex, Links: p, Assets: sink, BestEffort: t.Final})
	if err != nil {
		st.Discard()
		p.metrics.Renders.WithLabelValues("error").Inc()
		p.writeError(ctx, t, err.Error())
		return fmt.Errorf("render: %w", err)
	}
	sum := sha256.Sum256(res.HTML)
	hash := hex.EncodeToString(sum[:])

	changed := !(upToDate && hash == e.ContentHash)
	if changed {
		if err := st.WriteFile("index.html", res.HTML); err != nil {
			st.Discard()
			p.writeError(ctx, t, "write: "+err.Error())
			return err
		}
		version, err := p.out.Commit(e.Slug, st)
		if err != nil {
			p.writeError(ctx, t, "write: "+err.Error())
			return fmt.Errorf("commit: %w", err)
		}
		e.Version = version
		e.Assets = sink.used
		p.metrics.Renders.WithLabelValues("written").Inc()
	} else {
		st.Discard()
		p.metrics.Renders.WithLabelValues("unchanged").Inc()
	}

	wasActive := e.Active
	e.Active = true
	e.Title = res.Title
	e.ContentHash = hash
	e.SourceModified = modified
	e.RenderVersion = render.Version
	if !wasActive {
		e.PublishedAt = p.now().UTC()
	}
	if err := p.store.Put(ctx, e); err != nil {
		return err
	}
	if !wasActive {
		p.audit("publish", e)
		p.refreshGauge(ctx)
	} else if changed {
		p.audit("update", e)
	}

	// write-back: only what differs from the row
	url := p.pageURL(e.Slug)
	set := map[string]any{}
	var unset []string
	if currentURL != url {
		set[keys.PublicURL] = url
	}
	if keys.PublishedAt != "" && (changed || row.Prop(keys.PublishedAt) == "") {
		set[keys.PublishedAt] = p.now().UTC().Format(time.RFC3339)
	}
	if keys.PublishError != "" {
		if msg := strings.Join(res.Warnings, "; "); msg != "" {
			if row.Prop(keys.PublishError) != msg {
				set[keys.PublishError] = msg
			}
		} else if row.Prop(keys.PublishError) != "" {
			unset = append(unset, keys.PublishError)
		}
	}
	if err := p.api.PatchProperties(ctx, t.SpaceID, t.ObjectID, set, unset); err != nil {
		if anyapi.IsClientError(err) {
			p.log.Error("write-back refused", "object_id", t.ObjectID, "err", err)
			return nil
		}
		return fmt.Errorf("write-back: %w", err)
	}
	if _, ok := set[keys.PublicURL]; ok && !t.Received.IsZero() {
		p.metrics.PublishLatency.Observe(time.Since(t.Received).Seconds())
	}
	return nil
}

func (p *Processor) writeError(ctx context.Context, t Task, msg string) {
	k := p.keys[t.SpaceID].PublishError
	if k == "" || t.Row.Prop(k) == msg {
		return
	}
	if err := p.api.PatchProperties(ctx, t.SpaceID, t.ObjectID, map[string]any{k: msg}, nil); err != nil {
		p.log.Warn("could not write publish error", "object_id", t.ObjectID, "err", err)
	}
}

// Unpublish takes the page offline and clears the bot properties.
func (p *Processor) Unpublish(ctx context.Context, spaceID, objectID string) error {
	e, err := p.store.Get(ctx, objectID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !e.Active {
		return nil
	}
	if err := p.out.Remove(e.Slug); err != nil {
		return fmt.Errorf("remove files: %w", err)
	}
	p.runHook(ctx, e)

	keys := p.keys[e.SpaceID]
	unset := []string{keys.PublicURL}
	for _, k := range []string{keys.PublishedAt, keys.PublishError} {
		if k != "" {
			unset = append(unset, k)
		}
	}
	if err := p.api.PatchProperties(ctx, e.SpaceID, objectID, nil, unset); err != nil {
		if !anyapi.IsClientError(err) {
			return fmt.Errorf("clear properties: %w", err)
		}
		// deleted or archived object: nothing left to clear
		p.log.Info("could not clear properties", "object_id", objectID, "err", err)
	}

	if p.opts.KeepSlugOnRepublish {
		e.Active = false
		e.Version = ""
		e.ContentHash = ""
		e.Assets = nil
		err = p.store.Put(ctx, e)
	} else {
		err = p.store.Delete(ctx, objectID)
	}
	if err != nil {
		return err
	}
	p.audit("unpublish", e)
	p.refreshGauge(ctx)
	return nil
}

func (p *Processor) runHook(ctx context.Context, e store.Entry) {
	if p.opts.OnUnpublishHook == "" {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(hctx, "/bin/sh", "-c", p.opts.OnUnpublishHook)
	cmd.Env = append(os.Environ(),
		"ANYPUB_SLUG="+e.Slug,
		"ANYPUB_URL="+p.pageURL(e.Slug),
		"ANYPUB_OBJECT_ID="+e.ObjectID,
		"ANYPUB_SPACE_ID="+e.SpaceID,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		p.log.Warn("on_unpublish hook failed", "slug", e.Slug, "err", err, "output", strings.TrimSpace(string(out)))
	}
}

func (p *Processor) audit(action string, e store.Entry) {
	p.log.Info("audit", "action", action, "object_id", e.ObjectID, "space", e.SpaceID, "slug", e.Slug)
}

func (p *Processor) refreshGauge(ctx context.Context) {
	if n, err := p.store.CountActive(ctx); err == nil {
		p.metrics.PublishedObjects.Set(float64(n))
	}
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// recoverSlug reuses the slug of an existing Public URL after state loss.
func (p *Processor) recoverSlug(ctx context.Context, current string) string {
	prefix := p.opts.BaseURL + "/p/"
	if !strings.HasPrefix(current, prefix) {
		return ""
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(current, prefix), "/")
	if !slugRe.MatchString(slug) {
		return ""
	}
	if taken, err := p.store.SlugExists(ctx, slug); err != nil || taken {
		return ""
	}
	return slug
}

// newSlug returns 128 random bits, base64url.
func (p *Processor) newSlug(ctx context.Context) (string, error) {
	for {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		s := base64.RawURLEncoding.EncodeToString(b[:])
		taken, err := p.store.SlugExists(ctx, s)
		if err != nil {
			return "", err
		}
		if !taken {
			return s, nil
		}
	}
}

// Sweep removes pages on disk that have no active state row. Call it before
// any worker runs: it also clears tmp/.
func (p *Processor) Sweep(ctx context.Context) error {
	entries, err := p.store.List(ctx, "")
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, e := range entries {
		if e.Active {
			keep[e.Slug] = true
		}
	}
	removed, err := p.out.Sweep(keep)
	for _, s := range removed {
		p.log.Info("removed orphaned page", "slug", s)
	}
	p.refreshGauge(ctx)
	return err
}

// ActiveIDs returns the published object ids of a space.
func (p *Processor) ActiveIDs(ctx context.Context, spaceID string) (map[string]bool, error) {
	entries, err := p.store.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, e := range entries {
		if e.Active {
			ids[e.ObjectID] = true
		}
	}
	return ids, nil
}

type assetSink struct {
	p     *Processor
	ctx   context.Context
	st    *publish.Staging
	prev  store.Entry
	space string
	base  string
	used  map[string]string
}

var extRe = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

func (s *assetSink) Asset(fileID, name, mimeType string) (string, error) {
	if fn, ok := s.used[fileID]; ok {
		return s.base + "a/" + fn, nil
	}
	// file objects are immutable: reuse the copy from the previous version
	if fn, ok := s.prev.Assets[fileID]; ok {
		if copied, err := s.st.CopyFromVersion(s.prev.Version, "a/"+fn); err == nil && copied {
			s.used[fileID] = fn
			return s.base + "a/" + fn, nil
		}
	}
	tmp := "a/.download-" + randomName()
	f, err := s.st.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	ct, err := s.p.api.DownloadFile(s.ctx, s.space, fileID, s.p.opts.MaxAssetBytes, io.MultiWriter(f, h))
	f.Close()
	if err != nil {
		_ = s.st.Remove(tmp)
		switch {
		case errors.Is(err, anyapi.ErrTooLarge):
			return "", &render.AssetError{Msg: fmt.Sprintf("größer als %d MB", s.p.opts.MaxAssetBytes>>20)}
		case anyapi.IsNotFound(err):
			return "", &render.AssetError{Msg: "Datei nicht gefunden"}
		}
		return "", err
	}
	fn := hex.EncodeToString(h.Sum(nil)) + extFor(name, mimeType, ct)
	if err := s.st.Rename(tmp, "a/"+fn); err != nil {
		return "", err
	}
	s.used[fileID] = fn
	return s.base + "a/" + fn, nil
}

func extFor(name, mimeType, contentType string) string {
	if ext := strings.ToLower(path.Ext(name)); extRe.MatchString(ext) {
		return ext
	}
	for _, m := range []string{mimeType, contentType} {
		if m == "" {
			continue
		}
		if exts, _ := mime.ExtensionsByType(strings.Split(m, ";")[0]); len(exts) > 0 && extRe.MatchString(exts[0]) {
			return exts[0]
		}
	}
	return ".bin"
}

func randomName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
