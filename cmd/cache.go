package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

// cachingSnipe wraps *snipe.Client and serves ListAllUsers from a local cache
// file when use-cache is enabled. The Snipe-IT user list is large and
// read-only, so caching it avoids re-paginating every user on each run; all
// other methods pass straight through. Models and manufacturers are
// intentionally NOT cached — they are created during syncs and must stay fresh.
type cachingSnipe struct {
	*snipe.Client
	useCache bool
	cacheDir string
	log      *logrus.Logger
}

func newCachingSnipe(c *snipe.Client, useCache bool, cacheDir string, log *logrus.Logger) *cachingSnipe {
	return &cachingSnipe{Client: c, useCache: useCache, cacheDir: cacheDir, log: log}
}

func (c *cachingSnipe) usersCachePath() string {
	return filepath.Join(c.cacheDir, "users.json")
}

// ListAllUsers returns cached users when use-cache is set and the cache is
// readable; otherwise it fetches from the API and refreshes the cache.
func (c *cachingSnipe) ListAllUsers(ctx context.Context) ([]snipe.User, error) {
	if c.useCache {
		if data, err := os.ReadFile(c.usersCachePath()); err == nil {
			var users []snipe.User
			if err := json.Unmarshal(data, &users); err == nil {
				c.log.WithField("users", len(users)).Info("loaded snipe-it users from cache")
				return users, nil
			}
		}
		c.log.Info("snipe-it user cache miss; fetching from API")
	}
	users, err := c.Client.ListAllUsers(ctx)
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(users); err == nil {
		if err := os.MkdirAll(c.cacheDir, 0o755); err == nil {
			if err := os.WriteFile(c.usersCachePath(), data, 0o644); err != nil {
				c.log.WithError(err).Debug("failed to write snipe-it user cache")
			}
		}
	}
	return users, nil
}
