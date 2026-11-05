// Package config loads anypub.yaml and applies ANYPUB_* environment overrides.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Anytype    Anytype    `yaml:"anytype"`
	Spaces     []Space    `yaml:"spaces"`
	Properties Properties `yaml:"properties"`
	Publish    Publish    `yaml:"publish"`
	State      State      `yaml:"state"`
	Hooks      Hooks      `yaml:"hooks"`
	Obs        Obs        `yaml:"observability"`
}

type Anytype struct {
	// APIAddr is the base URL of the local HTTP API of anytype-cli (port 31012).
	APIAddr string `yaml:"api_addr"`
	// APIKeyFile holds the key created with `anytype auth apikey create`.
	APIKeyFile string `yaml:"api_key_file"`
	// APIKey may be set directly (env ANYPUB_ANYTYPE_API_KEY); the file wins if both are set.
	APIKey string `yaml:"api_key"`
	// TagProperty is the property key of the tag relation (v2 slug).
	TagProperty string `yaml:"tag_property"`
	// Heartbeat is the SSE keepalive cadence requested from the server.
	Heartbeat time.Duration `yaml:"heartbeat"`
}

type Space struct {
	ID  string `yaml:"id"`
	Tag string `yaml:"tag"`
}

type Properties struct {
	PublicURL    string `yaml:"public_url"`
	PublishedAt  string `yaml:"published_at"`
	PublishError string `yaml:"publish_error"`
	// Create missing properties on start.
	Create bool `yaml:"create"`
}

type Publish struct {
	BaseURL             string        `yaml:"base_url"`
	OutDir              string        `yaml:"out_dir"`
	DefaultMode         string        `yaml:"default_mode"`
	KeepSlugOnRepublish bool          `yaml:"keep_slug_on_republish"`
	NoIndex             bool          `yaml:"noindex"`
	FileGroup           string        `yaml:"file_group"`
	Debounce            time.Duration `yaml:"debounce"`
	Workers             int           `yaml:"workers"`
	MaxAssetBytes       int64         `yaml:"max_asset_bytes"`
	SafetyReconcile     time.Duration `yaml:"safety_reconcile"`
	MaxRetries          int           `yaml:"max_retries"`
}

type State struct {
	Path string `yaml:"path"`
}

type Hooks struct {
	OnUnpublish string `yaml:"on_unpublish"`
}

type Obs struct {
	ListenAddr string `yaml:"listen_addr"`
	LogLevel   string `yaml:"log_level"`
}

func Default() Config {
	return Config{
		Anytype: Anytype{
			APIAddr:     "http://127.0.0.1:31012",
			TagProperty: "tag",
			Heartbeat:   30 * time.Second,
		},
		Properties: Properties{
			PublicURL:    "Public URL",
			PublishedAt:  "Published At",
			PublishError: "Publish Error",
			Create:       true,
		},
		Publish: Publish{
			OutDir:          "/srv/anypub/public",
			DefaultMode:     "slug",
			NoIndex:         true,
			Debounce:        1500 * time.Millisecond,
			Workers:         4,
			MaxAssetBytes:   25 << 20,
			SafetyReconcile: time.Hour,
			MaxRetries:      5,
		},
		State: State{Path: "/var/lib/anypub/state.db"},
		Obs:   Obs{ListenAddr: "127.0.0.1:9464", LogLevel: "info"},
	}
}

// Load reads the YAML file (optional when path is empty), applies env
// overrides and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := applyEnv(&cfg, os.LookupEnv); err != nil {
		return cfg, err
	}
	if cfg.Anytype.APIKeyFile != "" {
		b, err := os.ReadFile(cfg.Anytype.APIKeyFile)
		if err != nil {
			return cfg, fmt.Errorf("read api key file: %w", err)
		}
		cfg.Anytype.APIKey = strings.TrimSpace(string(b))
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	var errs []error
	if c.Anytype.APIKey == "" {
		errs = append(errs, errors.New("anytype.api_key_file (or ANYPUB_ANYTYPE_API_KEY) is required"))
	}
	if len(c.Spaces) == 0 {
		errs = append(errs, errors.New("spaces: at least one space must be configured (opt-in)"))
	}
	seen := map[string]bool{}
	for i, s := range c.Spaces {
		if s.ID == "" {
			errs = append(errs, fmt.Errorf("spaces[%d].id is empty", i))
		}
		if seen[s.ID] {
			errs = append(errs, fmt.Errorf("spaces[%d]: duplicate id %s", i, s.ID))
		}
		seen[s.ID] = true
	}
	u, err := url.Parse(c.Publish.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("publish.base_url must be an absolute http(s) URL, got %q", c.Publish.BaseURL))
	}
	if c.Publish.DefaultMode != "slug" {
		errs = append(errs, fmt.Errorf("publish.default_mode %q is not supported yet (only slug; zk is planned)", c.Publish.DefaultMode))
	}
	if c.Publish.OutDir == "" {
		errs = append(errs, errors.New("publish.out_dir is required"))
	}
	if c.Publish.Workers < 1 {
		errs = append(errs, errors.New("publish.workers must be >= 1"))
	}
	if c.Properties.PublicURL == "" {
		errs = append(errs, errors.New("properties.public_url is required"))
	}
	if c.State.Path == "" {
		errs = append(errs, errors.New("state.path is required"))
	}
	return errors.Join(errs...)
}

// SpaceTag returns the tag configured for a space.
func (s Space) TagName() string {
	if s.Tag == "" {
		return "publish"
	}
	return s.Tag
}

// applyEnv walks the struct and overrides scalar fields from ANYPUB_<SECTION>_<FIELD>.
// ANYPUB_SPACES takes a comma-separated list of id[:tag].
func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	if v, ok := lookup("ANYPUB_SPACES"); ok && v != "" {
		cfg.Spaces = nil
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, tag, _ := strings.Cut(part, ":")
			cfg.Spaces = append(cfg.Spaces, Space{ID: id, Tag: tag})
		}
	}
	return walk(reflect.ValueOf(cfg).Elem(), "ANYPUB", lookup)
}

var durationType = reflect.TypeOf(time.Duration(0))

func walk(v reflect.Value, prefix string, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + "_" + strings.ToUpper(tag)
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct {
			if err := walk(fv, name, lookup); err != nil {
				return err
			}
			continue
		}
		raw, ok := lookup(name)
		if !ok {
			continue
		}
		switch {
		case fv.Type() == durationType:
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fv.SetInt(int64(d))
		case fv.Kind() == reflect.String:
			fv.SetString(raw)
		case fv.Kind() == reflect.Bool:
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fv.SetBool(b)
		case fv.Kind() == reflect.Int || fv.Kind() == reflect.Int64:
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fv.SetInt(n)
		}
	}
	return nil
}
