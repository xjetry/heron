package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

func runSecurityReset(args []string) error {
	fs := flag.NewFlagSet("security-reset", flag.ContinueOnError)
	db := fs.String("db", "heron.db", "SQLite database path")
	yes := fs.Bool("yes", false, "confirm removal of TOTP, recovery codes and Passkeys")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes || fs.NArg() != 0 {
		return errors.New("security-reset requires --yes and accepts no positional arguments")
	}
	st, a, err := openOffline(*db, false)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := a.ResetSecurity(context.Background()); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "TOTP, recovery codes and Passkeys removed; all sessions revoked; password and API tokens unchanged")
	return nil
}
