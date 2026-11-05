package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sker65/anypub/internal/anyapi"
)

// fakeAnytype mimics the parts of the Anytype HTTP API v2 anypub uses.
// A PATCH stamps last_modified_by with the bot, like heart does.
type fakeAnytype struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string]*fakeObj
	props   map[string][]anyapi.Property // per space
	files   map[string][]byte
	subs    map[*fakeSub]bool
	gets    map[string]int
	patches []string // "objectID:key" for every key set/unset
	clock   int64
	// options holds the tag options per space; like heart, an option only
	// comes into existence when an object write uses it
	options map[string]map[string]bool
}

type fakeObj struct {
	ID      string
	Space   string
	Name    string
	Tags    []string
	Blocks  []anyapi.Block
	Props   map[string]any
	Mod     int64
	ModBy   string
	Deleted bool
}

type fakeSub struct {
	space  string
	tag    string
	fields []string
	ch     chan anyapi.StreamEvent
	done   chan struct{}
	set    map[string]map[string]any // id -> last sent props
}

const (
	botRef  = "participant-bot#Bot"
	userRef = "participant-user#Stefan"
)

func newFake() *fakeAnytype {
	f := &fakeAnytype{
		objects: map[string]*fakeObj{},
		props:   map[string][]anyapi.Property{},
		files:   map[string][]byte{},
		subs:    map[*fakeSub]bool{},
		gets:    map[string]int{},
		clock:   1_700_000_000,
		options: map[string]map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeAnytype) Close() {
	f.DropStreams()
	f.srv.Close()
}

func (f *fakeAnytype) addSpace(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.props[id] = []anyapi.Property{
		{Key: "tag", Name: "Tag", Format: "multi_select"},
		{Key: "last_modified_date", Name: "Last modified date", Format: "date"},
		{Key: "last_modified_by", Name: "Last modified by", Format: "objects"},
	}
}

func (f *fakeAnytype) put(o *fakeObj) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock++
	o.Mod, o.ModBy = f.clock, userRef
	if o.Props == nil {
		o.Props = map[string]any{}
	}
	f.objects[o.ID] = o
	f.notifyLocked(o)
}

// mutate applies a user change.
func (f *fakeAnytype) mutate(id string, fn func(o *fakeObj)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[id]
	fn(o)
	f.clock++
	o.Mod, o.ModBy = f.clock, userRef
	f.notifyLocked(o)
}

func (f *fakeAnytype) prop(id, key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[id].Props[key]
}

func (f *fakeAnytype) getCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[id]
}

func (f *fakeAnytype) patchedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patches...)
}

// DropStreams closes every open stream, as a network failure would.
func (f *fakeAnytype) DropStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.subs {
		close(s.done)
		delete(f.subs, s)
	}
}

func (f *fakeAnytype) matches(s *fakeSub, o *fakeObj) bool {
	if o.Deleted || o.Space != s.space {
		return false
	}
	for _, t := range o.Tags {
		if t == s.tag {
			return true
		}
	}
	return false
}

func (f *fakeAnytype) row(s *fakeSub, o *fakeObj) *anyapi.Row {
	props := map[string]any{}
	for _, k := range s.fields {
		switch k {
		case "tag":
			props[k] = o.Tags
		case "last_modified_date":
			props[k] = time.Unix(o.Mod, 0).UTC().Format(time.RFC3339)
		case "last_modified_by":
			props[k] = o.ModBy
		default:
			if v, ok := o.Props[k]; ok {
				props[k] = v
			}
		}
	}
	return &anyapi.Row{ID: o.ID, Name: o.Name, Type: "page", Properties: props}
}

func (f *fakeAnytype) notifyLocked(o *fakeObj) {
	for _, t := range o.Tags {
		if f.options[o.Space] == nil {
			f.options[o.Space] = map[string]bool{}
		}
		f.options[o.Space][t] = true
	}
	for s := range f.subs {
		_, had := s.set[o.ID]
		now := f.matches(s, o)
		switch {
		case !had && now:
			r := f.row(s, o)
			s.set[o.ID] = r.Properties
			s.send(anyapi.StreamEvent{Type: anyapi.EventObjectAdded, Object: r})
		case had && !now:
			delete(s.set, o.ID)
			s.send(anyapi.StreamEvent{Type: anyapi.EventObjectRemoved, ObjectID: o.ID})
		case had && now:
			r := f.row(s, o)
			if fmt.Sprint(r.Properties) != fmt.Sprint(s.set[o.ID]) {
				s.set[o.ID] = r.Properties
				s.send(anyapi.StreamEvent{Type: anyapi.EventObjectUpdated, Object: r})
			}
		}
	}
}

func (s *fakeSub) send(ev anyapi.StreamEvent) {
	ev.SpaceID = s.space
	select {
	case s.ch <- ev:
	case <-s.done:
	}
}

var (
	spaceRe = regexp.MustCompile(`^/v2/spaces/([^/]+)(/.*)?$`)
	tagRe   = regexp.MustCompile(`IN \("([^"]+)"\)`)
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, 404, map[string]any{"status": 404, "code": "not_found", "message": "not found"})
}

func (f *fakeAnytype) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-key" {
		writeJSON(w, 401, map[string]any{"status": 401, "code": "unauthorized", "message": "bad key"})
		return
	}
	if r.URL.Path == "/v2/auth/whoami" {
		writeJSON(w, 200, map[string]any{"grant": map[string]any{"scoped": false}})
		return
	}
	m := spaceRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		notFound(w)
		return
	}
	space, rest := m[1], m[2]
	f.mu.Lock()
	_, known := f.props[space]
	f.mu.Unlock()
	if !known {
		notFound(w)
		return
	}
	switch {
	case rest == "/members/me":
		writeJSON(w, 200, anyapi.Member{ID: "participant-bot", Identity: "bot", Name: "Bot", Role: "writer"})
	case rest == "/properties" && r.Method == http.MethodGet:
		f.mu.Lock()
		writeJSON(w, 200, map[string]any{"data": f.props[space], "has_more": false})
		f.mu.Unlock()
	case rest == "/properties" && r.Method == http.MethodPost:
		var body struct{ Name, Format string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		p := anyapi.Property{Key: strings.ToLower(strings.ReplaceAll(body.Name, " ", "_")), Name: body.Name, Format: body.Format}
		f.mu.Lock()
		f.props[space] = append(f.props[space], p)
		f.mu.Unlock()
		writeJSON(w, 201, p)
	case rest == "/properties/tag/options":
		f.mu.Lock()
		var data []map[string]string
		for name := range f.options[space] {
			if strings.HasPrefix(strings.ToLower(name), strings.ToLower(r.URL.Query().Get("prefix"))) {
				data = append(data, map[string]string{"name": name})
			}
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"data": data, "has_more": false})
	case rest == "/search/stream":
		f.stream(w, r, space)
	case strings.HasPrefix(rest, "/objects/"):
		id := strings.TrimPrefix(rest, "/objects/")
		if r.Method == http.MethodPatch {
			f.patch(w, r, id)
		} else {
			f.get(w, id)
		}
	case strings.HasPrefix(rest, "/files/") && strings.HasSuffix(rest, "/content"):
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "/files/"), "/content")
		f.mu.Lock()
		b, ok := f.files[id]
		f.mu.Unlock()
		if !ok {
			notFound(w)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(b)
	default:
		notFound(w)
	}
}

func (f *fakeAnytype) get(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[id]
	if !ok || o.Deleted {
		notFound(w)
		return
	}
	f.gets[id]++
	props := map[string]any{"name": o.Name}
	for k, v := range o.Props {
		props[k] = v
	}
	writeJSON(w, 200, anyapi.Document{ID: o.ID, Type: "page", Properties: props, Blocks: o.Blocks})
}

func (f *fakeAnytype) patch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Ops []struct {
			Op    string         `json:"op"`
			Set   map[string]any `json:"set"`
			Unset []string       `json:"unset"`
		} `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"status": 400, "message": err.Error()})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[id]
	if !ok || o.Deleted {
		notFound(w)
		return
	}
	for _, op := range body.Ops {
		for k, v := range op.Set {
			o.Props[k] = v
			f.patches = append(f.patches, id+":"+k)
		}
		for _, k := range op.Unset {
			delete(o.Props, k)
			f.patches = append(f.patches, id+":"+k)
		}
	}
	f.clock++
	o.Mod, o.ModBy = f.clock, botRef
	f.notifyLocked(o)
	writeJSON(w, 200, map[string]any{"etag": "x"})
}

func (f *fakeAnytype) stream(w http.ResponseWriter, r *http.Request, space string) {
	var body anyapi.SearchRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	tm := tagRe.FindStringSubmatch(body.Filter)
	if tm == nil {
		writeJSON(w, 400, map[string]any{"status": 400, "code": "validation_failed", "message": "bad filter"})
		return
	}
	f.mu.Lock()
	known := f.options[space][tm[1]]
	f.mu.Unlock()
	if !known {
		writeJSON(w, 400, map[string]any{"status": 400, "code": "validation_failed", "message": "invalid filter",
			"issues": []map[string]string{{"path": "/filter", "message": fmt.Sprintf("property \"tag\" has no option named %q", tm[1])}}})
		return
	}
	s := &fakeSub{space: space, tag: tm[1], fields: body.Fields, ch: make(chan anyapi.StreamEvent, 64), done: make(chan struct{}), set: map[string]map[string]any{}}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	write := func(ev anyapi.StreamEvent) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
		fl.Flush()
	}

	f.mu.Lock()
	var snapshot []anyapi.StreamEvent
	for _, o := range f.objects {
		if f.matches(s, o) {
			row := f.row(s, o)
			s.set[o.ID] = row.Properties
			snapshot = append(snapshot, anyapi.StreamEvent{Type: anyapi.EventObjectAdded, SpaceID: space, Object: row})
		}
	}
	f.subs[s] = true
	f.mu.Unlock()

	for _, ev := range snapshot {
		write(ev)
	}
	write(anyapi.StreamEvent{Type: anyapi.EventSnapshotComplete, SpaceID: space})

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-s.ch:
			write(ev)
		case <-tick.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		case <-s.done:
			return
		case <-r.Context().Done():
			f.mu.Lock()
			delete(f.subs, s)
			f.mu.Unlock()
			return
		}
	}
}
