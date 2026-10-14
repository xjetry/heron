package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"
)

func runWindow(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: heron-hub window open|close|show [flags]")
	}
	fs := flag.NewFlagSet("window "+args[0], flag.ContinueOnError)
	db := fs.String("db", "heron.db", "SQLite database path")
	ttl := fs.Duration("ttl", time.Hour, "how long the window stays open (open)")
	maxNodes := fs.Int("max", 10, "how many nodes may register (open)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	st, a, err := openOffline(*db, args[0] == "open")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	switch args[0] {
	case "open":
		key, until, err := a.OpenWindow(ctx, *ttl, *maxNodes)
		if err != nil {
			return err
		}
		fmt.Printf("key: %s\nexpires: %s\nmax: %d\n", key, until.UTC().Format(time.RFC3339), *maxNodes)
	case "close":
		return a.CloseWindow(ctx)
	case "show":
		w, ok, err := a.Window(ctx)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("no window")
			return nil
		}
		fmt.Printf("expires: %s\nremaining: %d\n", w.ExpiresAt.Format(time.RFC3339), w.Remaining)
	default:
		return fmt.Errorf("unknown window command %q", args[0])
	}
	return nil
}
