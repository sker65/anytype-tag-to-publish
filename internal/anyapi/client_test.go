package anyapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPatchPropertiesAllowlist(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("mutation without Idempotency-Key")
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	c.AllowWrites("public_url", "published_at")

	for _, tc := range []struct {
		set   map[string]any
		unset []string
	}{
		{set: map[string]any{"name": "pwned"}},
		{set: map[string]any{"public_url": "x", "description": "y"}},
		{unset: []string{"tag"}},
	} {
		err := c.PatchProperties(context.Background(), "s", "o", tc.set, tc.unset)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("set=%v unset=%v: err = %v, want ErrNotAllowed", tc.set, tc.unset, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests sent for refused writes", calls.Load())
	}
	if err := c.PatchProperties(context.Background(), "s", "o", map[string]any{"public_url": "x"}, []string{"published_at"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("allowed write was not sent")
	}
}

func TestStreamParsesEventsAndHeartbeats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("heartbeat") != "1" {
			t.Errorf("heartbeat query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, ": hello\n\n")
		fmt.Fprint(w, "event: object_added\ndata: {\"type\":\"object_added\",\"space_id\":\"s\",\n")
		fmt.Fprint(w, "data: \"object\":{\"id\":\"o1\",\"name\":\"N\",\"properties\":{\"last_modified_date\":\"2026-11-05T10:00:00Z\"}}}\n\n")
		fmt.Fprint(w, "event: snapshot_complete\r\ndata: {\"space_id\":\"s\"}\r\n\r\n")
		fl.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	var got []StreamEvent
	errStop := errors.New("stop")
	err := c.Stream(context.Background(), "s", SearchRequest{Filter: `tag IN ("publish")`}, time.Second, func(ev StreamEvent) error {
		got = append(got, ev)
		if ev.Type == EventSnapshotComplete {
			return errStop
		}
		return nil
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 || got[0].Object.ID != "o1" || got[0].Object.Prop("last_modified_date") != "2026-11-05T10:00:00Z" || got[1].Type != EventSnapshotComplete {
		t.Fatalf("events = %+v", got)
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // silent: no heartbeats
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	start := time.Now()
	err := c.Stream(context.Background(), "s", SearchRequest{}, time.Second, func(StreamEvent) error { return nil })
	if !errors.Is(err, errIdle) {
		t.Fatalf("err = %v, want idle timeout", err)
	}
	if d := time.Since(start); d < 2*time.Second || d > 5*time.Second {
		t.Fatalf("idle timeout after %v", d)
	}
}

func TestErrorEnvelopeAndDownloadLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			w.Write([]byte(strings.Repeat("x", 100)))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"status":404,"code":"not_found","message":"no such object"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	_, err := c.GetObject(context.Background(), "s", "o")
	if !IsNotFound(err) || !strings.Contains(err.Error(), "no such object") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.DownloadFile(context.Background(), "s", "f", 10, io.Discard); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("download err = %v", err)
	}
	if _, err := c.DownloadFile(context.Background(), "s", "f", 0, io.Discard); err != nil {
		t.Fatalf("unlimited download: %v", err)
	}
}

func TestMemberIs(t *testing.T) {
	m := Member{ID: "participant-abc", Identity: "abc"}
	for ref, want := range map[string]bool{
		"participant-abc#Bot": true,
		"participant-abc":     true,
		`["participant-abc"]`: true,
		"participant-xyz#Bot": false,
		"":                    false,
	} {
		if m.Is(ref) != want {
			t.Errorf("Is(%q) = %v", ref, !want)
		}
	}
}

// A server that never sends response headers must not stall the stream.
func TestStreamIdleBeforeHeaders(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	c := New(srv.URL, "k")
	start := time.Now()
	err := c.Stream(context.Background(), "s", SearchRequest{}, time.Second, func(StreamEvent) error { return nil })
	if !errors.Is(err, errIdle) {
		t.Fatalf("err = %v, want idle timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("idle timeout after %v", d)
	}
}

func TestOptionExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/spaces/s/properties/tag/options" || r.URL.Query().Get("prefix") != "publish" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write([]byte(`{"data":[{"name":"publishing"},{"name":"Publish"}],"has_more":false}`))
	}))
	defer srv.Close()
	ok, err := New(srv.URL, "k").OptionExists(context.Background(), "s", "tag", "publish")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}
