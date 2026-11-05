package anyapi

import (
	"encoding/json"
	"fmt"
)

// Row is a search row as the v2 search and search stream return it.
type Row struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	SpaceID    string         `json:"space_id,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
}

// Prop returns a property value of the row as a comparable string ("" when absent).
func (r Row) Prop(key string) string {
	if r.Properties == nil {
		return ""
	}
	return scalarString(r.Properties[key])
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprintf("%.0f", x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// Stream event types of POST /v2/spaces/{space_id}/search/stream.
const (
	EventObjectAdded      = "object_added"
	EventObjectUpdated    = "object_updated"
	EventObjectRemoved    = "object_removed"
	EventSnapshotComplete = "snapshot_complete"
)

type StreamEvent struct {
	Type     string            `json:"type"`
	SpaceID  string            `json:"space_id"`
	Object   *Row              `json:"object,omitempty"`
	ObjectID string            `json:"object_id,omitempty"`
	Warnings []json.RawMessage `json:"warnings,omitempty"`
}

// SearchRequest is the body of the space search and the search stream.
type SearchRequest struct {
	Filter string   `json:"filter,omitempty"`
	Fields []string `json:"fields,omitempty"`
}

type Property struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Format string `json:"format"`
}

// Document is an object read in the AnyBlock v2 JSON shape (GET …/objects/{id}).
type Document struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Etag       string         `json:"etag"`
	Icon       *Icon          `json:"icon,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
	Blocks     []Block        `json:"blocks,omitempty"`
}

func (d *Document) Name() string {
	if s, ok := d.Properties["name"].(string); ok {
		return s
	}
	return ""
}

func (d *Document) Description() string {
	if s, ok := d.Properties["description"].(string); ok {
		return s
	}
	return ""
}

type Icon struct {
	Format string `json:"format"`
	Emoji  string `json:"emoji,omitempty"`
	File   string `json:"file,omitempty"`
	Name   string `json:"name,omitempty"`
}

// Block is one entry of the flat, pre-order blocks array (SPEC §4/§5).
type Block struct {
	Indent          int    `json:"indent,omitempty"`
	ID              string `json:"id,omitempty"`
	Type            string `json:"type"`
	Text            string `json:"text,omitempty"`
	Checked         bool   `json:"checked,omitempty"`
	Color           string `json:"color,omitempty"`
	BackgroundColor string `json:"background_color,omitempty"`
	Align           string `json:"align,omitempty"`
	Language        string `json:"language,omitempty"`
	ObjectID        string `json:"object_id,omitempty"`
	Hash            string `json:"hash,omitempty"`
	Name            string `json:"name,omitempty"`
	MimeType        string `json:"mime_type,omitempty"`
	Size            int64  `json:"size,omitempty"`
	URL             string `json:"url,omitempty"`
	Processor       string `json:"processor,omitempty"`
	Style           string `json:"style,omitempty"`
	Icon            *Icon  `json:"icon,omitempty"`

	Columns []TableColumn `json:"columns,omitempty"`
	Rows    []TableRow    `json:"rows,omitempty"`
}

// FileID returns the target file object of a file block (legacy hash as fallback).
func (b Block) FileID() string {
	if b.ObjectID != "" {
		return b.ObjectID
	}
	return b.Hash
}

type TableColumn struct {
	ID    string `json:"id,omitempty"`
	Width int    `json:"width,omitempty"`
}

// TableRow cells are a string, null, a block object or an array of flat blocks (SPEC §6.1).
type TableRow struct {
	ID       string            `json:"id,omitempty"`
	IsHeader bool              `json:"is_header,omitempty"`
	Cells    []json.RawMessage `json:"cells,omitempty"`
}
