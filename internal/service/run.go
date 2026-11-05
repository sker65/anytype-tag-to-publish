// Package service wires config, API client, store, processor and watcher.
package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sker65/anypub/internal/anyapi"
	"github.com/sker65/anypub/internal/app"
	"github.com/sker65/anypub/internal/config"
	"github.com/sker65/anypub/internal/obs"
	"github.com/sker65/anypub/internal/publish"
	"github.com/sker65/anypub/internal/store"
	"github.com/sker65/anypub/internal/watcher"
)

// Run blocks until ctx ends or startup fails.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	st, err := store.Open(cfg.State.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	out, err := publish.NewWriter(cfg.Publish.OutDir, cfg.Publish.FileGroup)
	if err != nil {
		return err
	}

	api := anyapi.New(cfg.Anytype.APIAddr, cfg.Anytype.APIKey)
	if who, err := api.WhoAmI(ctx); err != nil {
		return fmt.Errorf("anytype api not reachable: %w", err)
	} else {
		log.Info("connected to anytype api", "grant", who["grant"])
	}

	keys := map[string]app.PropKeys{}
	var spaces []watcher.Space
	var allowed []string
	for _, sc := range cfg.Spaces {
		s, k, err := PrepareSpace(ctx, api, sc, cfg, log)
		if err != nil {
			return fmt.Errorf("space %s: %w", sc.ID, err)
		}
		keys[sc.ID] = k
		spaces = append(spaces, s)
		allowed = append(allowed, k.PublicURL, k.PublishedAt, k.PublishError)
	}
	api.AllowWrites(allowed...)

	metrics := obs.NewMetrics()
	proc := app.NewProcessor(api, st, out, keys, app.Options{
		BaseURL:             cfg.Publish.BaseURL,
		NoIndex:             cfg.Publish.NoIndex,
		KeepSlugOnRepublish: cfg.Publish.KeepSlugOnRepublish,
		MaxAssetBytes:       cfg.Publish.MaxAssetBytes,
		OnUnpublishHook:     cfg.Hooks.OnUnpublish,
	}, metrics, log)
	if err := proc.Sweep(ctx); err != nil {
		return fmt.Errorf("sweep: %w", err)
	}

	q := watcher.NewQueue(proc.Handle, cfg.Publish.MaxRetries, log)
	q.Start(ctx, cfg.Publish.Workers)

	w := watcher.New(api, q, proc, watcher.Config{
		Heartbeat:       cfg.Anytype.Heartbeat,
		Debounce:        cfg.Publish.Debounce,
		SafetyReconcile: cfg.Publish.SafetyReconcile,
		OptionPoll:      cfg.Anytype.Heartbeat,
	}, metrics, log)

	go func() {
		if err := obs.Serve(ctx, cfg.Obs.ListenAddr, metrics, func() bool { return w.Ready(spaces) }); err != nil {
			log.Error("health server", "err", err)
		}
	}()

	log.Info("anypub started", "spaces", len(spaces), "out_dir", cfg.Publish.OutDir)
	w.Run(ctx, spaces)
	q.Wait()
	return nil
}
