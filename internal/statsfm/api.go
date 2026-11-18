package statsfm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var errFormat = errors.New("unexpected API response")

const apiBase = "https://api.stats.fm/api/v1/users/"

type object = map[string]any
type client struct {
	base string
	http *http.Client
	pace time.Duration
	last time.Time
}

type apiError struct {
	code    int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.message) }

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func retryDelay(header string, fallback time.Duration) time.Duration {
	if seconds, err := strconv.ParseUint(header, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(header); err == nil {
		return max(0, time.Until(date))
	}
	return fallback
}
func (c *client) get(ctx context.Context, path string, q url.Values) (any, error) {
	target := c.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stats-fm-export/1.0")
	req.Header.Set("Accept", "application/json")
	for attempt := 0; ; attempt++ {
		if err := wait(ctx, time.Until(c.last.Add(c.pace))); err != nil {
			return nil, err
		}
		c.last = time.Now()
		resp, err := c.http.Do(req)
		delay := time.Second << attempt
		if err == nil {
			var data []byte
			// Responses are per page. Bound a malformed or unexpectedly large response.
			const maxBody = 64 << 20
			data, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
			resp.Body.Close()
			if err == nil {
				if len(data) > maxBody {
					return nil, fmt.Errorf("%w: response exceeds 64 MiB", errFormat)
				}
				if resp.StatusCode == http.StatusNoContent {
					return nil, nil
				}
				if resp.StatusCode/100 == 2 {
					var value any
					dec := json.NewDecoder(bytes.NewReader(data))
					dec.UseNumber()
					if !json.Valid(data) {
						return nil, fmt.Errorf("%w: expected JSON", errFormat)
					}
					err = dec.Decode(&value)
					return value, err
				}
				var problem struct{ Message string }
				_ = json.Unmarshal(data, &problem)
				err = &apiError{resp.StatusCode, problem.Message}
				if resp.StatusCode != 429 && resp.StatusCode < 500 {
					return nil, err
				}
				delay = retryDelay(resp.Header.Get("Retry-After"), delay)
			}
		}
		if attempt >= 3 {
			return nil, err
		}
		if err := wait(ctx, delay); err != nil {
			return nil, err
		}
	}
}

type profile struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Plus        *bool    `json:"plus"`
	Imported    *bool    `json:"has_imported"`
	Orders      []string `json:"ranking_methods"`
}

func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\?#\x00\r\n")
}
func (c *client) profile(ctx context.Context, username string) (profile, error) {
	var p profile
	if !safeSegment(username) {
		return p, errors.New("invalid username")
	}
	v, err := c.get(ctx, url.PathEscape(username), nil)
	if err != nil {
		return p, err
	}
	envelope, _ := v.(object)
	item, _ := envelope["item"].(object)
	p.ID, _ = item["id"].(string)
	if !safeSegment(p.ID) {
		return p, errors.New("profile has no valid ID")
	}
	p.Username, _ = item["customId"].(string)
	if p.Username == "" {
		p.Username = username
	}
	p.DisplayName, _ = item["displayName"].(string)
	if b, ok := item["isPlus"].(bool); ok {
		p.Plus = &b
	}
	if b, ok := item["hasImported"].(bool); ok {
		p.Imported = &b
	}
	p.Orders = []string{"COUNT", "TIME"}
	if item["spotifyAuth"] != nil || item["orderBy"] == "SPOTIFY" {
		p.Orders = append(p.Orders, "SPOTIFY")
	}
	if item["appleMusicAuth"] != nil || item["orderBy"] == "APPLEMUSIC" {
		p.Orders = append(p.Orders, "APPLEMUSIC")
	}
	if len(p.Orders) == 2 {
		p.Orders = append(p.Orders, "default")
	}
	return p, nil
}
