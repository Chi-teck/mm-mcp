package mattermost

import (
	"context"
	"strconv"

	"github.com/mattermost/mattermost/server/public/model"
)

// MaxFileSize returns the server's upload limit in bytes from the client
// config. ok is false when the limit can't be read (request failure, missing
// or malformed value); callers then skip the size check.
// A success is cached for CacheTTL; failures are not cached.
func (c *Context) MaxFileSize(ctx context.Context) (size int64, ok bool) {
	now := c.now()
	c.mu.Lock()
	e := c.maxFile
	c.mu.Unlock()
	if now.Before(e.expires) {
		return e.val, true
	}

	// Client4.GetClientConfig omits format=old, which servers before v11 require (501 without it);
	// v11+ ignores it.
	resp, err := c.client.DoAPIGet(ctx, "/config/client?format=old", "")
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	cfg, _, err := model.DecodeJSONFromResponse[map[string]string](resp)
	if err != nil {
		return 0, false
	}
	size, err = strconv.ParseInt(cfg["MaxFileSize"], 10, 64)
	if err != nil || size <= 0 {
		return 0, false
	}

	c.mu.Lock()
	c.maxFile = cached[int64]{val: size, expires: now.Add(CacheTTL)}
	c.mu.Unlock()
	return size, true
}
