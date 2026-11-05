package anyapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errIdle = errors.New("anyapi: stream idle timeout")

// Stream opens POST /v2/spaces/{space}/search/stream and calls fn for every
// event until ctx ends, the server closes the connection, fn returns an error,
// or nothing (not even a heartbeat) arrives for three heartbeat periods.
// It always returns a non-nil error.
func (c *Client) Stream(ctx context.Context, spaceID string, sr SearchRequest, heartbeat time.Duration, fn func(StreamEvent) error) error {
	if heartbeat <= 0 {
		heartbeat = 30 * time.Second
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	q := url.Values{"heartbeat": {fmt.Sprint(int(heartbeat.Seconds()))}}
	req, err := c.newRequest(ctx, http.MethodPost, "/v2/spaces/"+url.PathEscape(spaceID)+"/search/stream", q, sr)
	if err != nil {
		return err
	}
	req.Header.Del("Idempotency-Key")
	req.Header.Set("Accept", "text/event-stream")
	// the watchdog also covers the wait for the response headers: a server
	// that accepts the connection but never answers must not stall us forever
	idle := time.AfterFunc(3*heartbeat, func() { cancel(errIdle) })
	defer idle.Stop()
	resp, err := c.http.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}

	r := bufio.NewReaderSize(resp.Body, 64<<10)
	var event string
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return fmt.Errorf("anyapi: stream closed: %w", err)
		}
		idle.Reset(3 * heartbeat)
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data.Len() == 0 {
				event = ""
				continue
			}
			var ev StreamEvent
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				return fmt.Errorf("anyapi: decode %s event: %w", event, err)
			}
			if ev.Type == "" {
				ev.Type = event
			}
			if ev.SpaceID == "" {
				ev.SpaceID = spaceID
			}
			event = ""
			data.Reset()
			if err := fn(ev); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			}
		}
	}
}
