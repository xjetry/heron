//go:build !linux

package update

import (
	"context"
	"errors"
	"log/slog"
)

func Serve(context.Context, string, *slog.Logger) error {
	return errors.New("online updates require Linux systemd")
}
