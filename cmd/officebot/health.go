package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"officebot/internal/storage"
)

// ------------------------------------------------------------------ health

type health struct {
	Time       int64 `json:"time"`
	Socket     bool  `json:"socket"`
	DownSince  int64 `json:"down_since,omitempty"`
	DatabaseOK bool  `json:"database_ok"`
}

const (
	healthInterval = 15 * time.Second
	socketGrace    = 2 * time.Minute  // healthcheck tolerates short reconnects
	socketGiveUp   = 15 * time.Minute // then exit so the restart policy kicks in
)

func healthWriter(ctx context.Context, path string, store *storage.Store, connected func() bool, cancel context.CancelCauseFunc, log *slog.Logger) {
	var downSince time.Time
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	for {
		now := time.Now()
		h := health{Time: now.Unix(), Socket: connected(), DatabaseOK: store.Ping(ctx) == nil}
		if h.Socket {
			downSince = time.Time{}
		} else {
			if downSince.IsZero() {
				downSince = now
			}
			h.DownSince = downSince.Unix()
			if now.Sub(downSince) > socketGiveUp {
				cancel(fmt.Errorf("socket mode disconnected for more than %s", socketGiveUp))
				return
			}
		}
		if b, err := json.Marshal(h); err == nil {
			if err := os.WriteFile(path, b, 0o600); err != nil {
				log.Warn("write health file", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func healthcheck() int {
	path := os.Getenv("HEALTH_FILE")
	if path == "" {
		path = "/tmp/officebot.health"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("unhealthy: no health file:", err)
		return 1
	}
	var h health
	if err := json.Unmarshal(b, &h); err != nil {
		fmt.Println("unhealthy: bad health file")
		return 1
	}
	now := time.Now()
	switch {
	case now.Sub(time.Unix(h.Time, 0)) > 4*healthInterval:
		fmt.Println("unhealthy: process not responding")
		return 1
	case !h.DatabaseOK:
		fmt.Println("unhealthy: database not available")
		return 1
	case !h.Socket && now.Sub(time.Unix(h.DownSince, 0)) > socketGrace:
		fmt.Println("unhealthy: slack socket mode disconnected since", time.Unix(h.DownSince, 0).Format(time.RFC3339))
		return 1
	}
	fmt.Println("healthy")
	return 0
}
