package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/xjetry/heron/internal/hub/store"
)

func runToken(args []string) error { return runTokenWith(args, os.Stdout, os.Stderr) }

// runTokenWith 直接改库。hub 每次请求都查 api_token、不缓存，所以运行中的 hub 不需要重启，
// 吊销在下一个请求即生效——这是面板不可用或管理员密码已泄漏时的应急路径。
func runTokenWith(args []string, out, errOut io.Writer) error {
	if len(args) < 1 {
		return errors.New("usage: heron-hub token list|revoke [flags]")
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ContinueOnError)
	fs.SetOutput(errOut)
	db := fs.String("db", "heron.db", "SQLite database path")
	id := fs.Int64("id", 0, "API token id (revoke)")
	all := fs.Bool("all", false, "revoke every API token (revoke)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	st, _, err := openOffline(*db, false)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	switch args[0] {
	case "list":
		list, err := st.ListAPITokens(ctx)
		if err != nil {
			return err
		}
		return printAPITokens(out, list)
	case "revoke":
		switch {
		case *all && *id != 0:
			return errors.New("--id and --all are mutually exclusive")
		case *all:
			n, err := st.DeleteAllAPITokens(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "revoked %d API tokens\n", n)
		case *id != 0:
			found, err := st.DeleteAPIToken(ctx, *id)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("API token %d does not exist", *id)
			}
			fmt.Fprintf(out, "revoked API token %d\n", *id)
		default:
			return errors.New("--id or --all is required")
		}
		fmt.Fprintln(errOut, "takes effect on the next request; the hub looks up API tokens on every call")
		return nil
	default:
		return fmt.Errorf("unknown token command %q; want list or revoke", args[0])
	}
}

func printAPITokens(w io.Writer, list []store.APIToken) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPERMISSIONS\tSCOPE\tCREATED\tLAST USED")
	for _, t := range list {
		used := "never"
		if !t.LastUsedAt.IsZero() {
			used = t.LastUsedAt.Format("2006-01-02 15:04Z")
		}
		permissions := make([]string, 0, len(t.Permissions))
		for _, p := range t.Permissions {
			permissions = append(permissions, string(p))
		}
		access := strings.Join(permissions, ",")
		if access == "" {
			access = "read-only"
		}
		scope := fmt.Sprint(t.NodeIDs)
		if t.AllNodes {
			scope = "all nodes"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, access, scope, t.CreatedAt.Format("2006-01-02 15:04Z"), used)
	}
	return tw.Flush()
}
