package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sker65/anypub/internal/anyapi"
	"github.com/sker65/anypub/internal/app"
	"github.com/sker65/anypub/internal/config"
	"github.com/sker65/anypub/internal/watcher"
)

// Bundled property keys the stream needs.
const (
	KeyLastModifiedDate = "last_modified_date"
	KeyLastModifiedBy   = "last_modified_by"
)

// SpaceAPI is what PrepareSpace needs.
type SpaceAPI interface {
	Me(ctx context.Context, spaceID string) (anyapi.Member, error)
	ListProperties(ctx context.Context, spaceID string) ([]anyapi.Property, error)
	CreateProperty(ctx context.Context, spaceID, name, format string) (anyapi.Property, error)
}

// PrepareSpace resolves the bot member and the property keys of a space and
// creates missing bot properties when allowed.
func PrepareSpace(ctx context.Context, api SpaceAPI, sc config.Space, cfg config.Config, log *slog.Logger) (watcher.Space, app.PropKeys, error) {
	me, err := api.Me(ctx, sc.ID)
	if err != nil {
		return watcher.Space{}, app.PropKeys{}, fmt.Errorf("resolve bot member (is the bot a member of this space?): %w", err)
	}
	if me.Role != "" && me.Role != "writer" && me.Role != "owner" && me.Role != "admin" {
		log.Warn("bot cannot write in this space; the URL write-back will fail", "space", sc.ID, "role", me.Role)
	}
	props, err := api.ListProperties(ctx, sc.ID)
	if err != nil {
		return watcher.Space{}, app.PropKeys{}, fmt.Errorf("list properties: %w", err)
	}
	byKey := map[string]anyapi.Property{}
	for _, p := range props {
		byKey[p.Key] = p
	}
	if _, ok := byKey[cfg.Anytype.TagProperty]; !ok {
		log.Warn("tag property not found in space; check anytype.tag_property", "space", sc.ID, "key", cfg.Anytype.TagProperty)
	}

	resolve := func(name, format string) (string, error) {
		if name == "" {
			return "", nil
		}
		for _, p := range props {
			if strings.EqualFold(p.Name, name) {
				if p.Format != "" && p.Format != format {
					log.Warn("property has an unexpected format", "space", sc.ID, "name", name, "format", p.Format, "want", format)
				}
				return p.Key, nil
			}
		}
		if !cfg.Properties.Create {
			return "", fmt.Errorf("property %q missing and properties.create is off", name)
		}
		p, err := api.CreateProperty(ctx, sc.ID, name, format)
		if err != nil {
			return "", fmt.Errorf("create property %q: %w", name, err)
		}
		log.Info("created property", "space", sc.ID, "name", name, "key", p.Key)
		return p.Key, nil
	}

	var k app.PropKeys
	if k.PublicURL, err = resolve(cfg.Properties.PublicURL, "url"); err != nil {
		return watcher.Space{}, k, err
	}
	if k.PublishedAt, err = resolve(cfg.Properties.PublishedAt, "date"); err != nil {
		return watcher.Space{}, k, err
	}
	if k.PublishError, err = resolve(cfg.Properties.PublishError, "text"); err != nil {
		return watcher.Space{}, k, err
	}

	fields := []string{cfg.Anytype.TagProperty, KeyLastModifiedDate, KeyLastModifiedBy, k.PublicURL}
	for _, f := range []string{k.PublishedAt, k.PublishError} {
		if f != "" {
			fields = append(fields, f)
		}
	}
	return watcher.Space{
		ID:       sc.ID,
		Filter:   watcher.TagFilter(cfg.Anytype.TagProperty, sc.TagName()),
		Fields:   fields,
		TagKey:   cfg.Anytype.TagProperty,
		TagName:  sc.TagName(),
		Bot:      me,
		ModByKey: KeyLastModifiedBy,
	}, k, nil
}
