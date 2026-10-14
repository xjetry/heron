package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xjetry/heron/internal/hub/theme"
)

type ThemeRestoreSummary struct {
	Restored []string `json:"restored"`
	Missing  []string `json:"missing"`
	Ignored  []string `json:"ignored"`
}

type themeReference struct{ id, digest, sha256 string }
type themeRestorePlan struct {
	refs     []themeReference
	packages map[themeReference][]byte
	ignored  []string
}

func readThemeReferences(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]themeReference, error) {
	rows, err := q.QueryContext(ctx, "SELECT theme_id,digest,sha256 FROM snapshot_theme ORDER BY theme_id,digest")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []themeReference
	for rows.Next() {
		var r themeReference
		if err := rows.Scan(&r.id, &r.digest, &r.sha256); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// 整个目录在目标打开前验证；事务只使用已校验的原包字节，不再读取可能被替换的文件。
func prepareThemeRestore(ctx context.Context, config, dir string) (*themeRestorePlan, error) {
	plan := &themeRestorePlan{packages: make(map[themeReference][]byte)}
	db, err := sql.Open("sqlite", dsn(config, "&mode=ro"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	plan.refs, err = readThemeReferences(ctx, db)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, r := range plan.refs {
		if !theme.ValidID(r.id) || (r.digest != "" && !validThemeDigest(r.digest)) || (r.sha256 != "" && r.sha256 != r.digest) {
			return nil, fmt.Errorf("invalid snapshot theme reference %q", r.id)
		}
		counts[r.id]++
		if counts[r.id] > MaxThemeVersions {
			return nil, fmt.Errorf("theme %q has too many versions", r.id)
		}
	}
	if len(counts) > theme.MaxThemes {
		return nil, fmt.Errorf("config snapshot has more than %d themes", theme.MaxThemes)
	}
	if dir == "" {
		return plan, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("themes directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("theme package %q is not a regular file", entry.Name())
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(f, theme.MaxPackageBytes+1))
		if err = errors.Join(err, f.Close()); err != nil {
			return nil, err
		}
		pkg, err := theme.Parse(content)
		if err != nil {
			return nil, fmt.Errorf("theme package %q: %w", entry.Name(), err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(content))
		name := strings.TrimSuffix(entry.Name(), ".zip")
		if name != digest && name != pkg.Manifest.ID {
			return nil, fmt.Errorf("theme package %q: filename does not match SHA256 or legacy theme id", entry.Name())
		}
		matched := false
		for _, r := range plan.refs {
			if r.id != pkg.Manifest.ID || (r.digest != "" && r.digest != digest) {
				continue
			}
			if r.digest != "" && name != digest {
				return nil, fmt.Errorf("theme package %q: SHA256 does not match filename", entry.Name())
			}
			plan.packages[r] = content
			matched = true
			break
		}
		if !matched {
			plan.ignored = append(plan.ignored, entry.Name())
		}
	}
	for _, r := range plan.refs {
		if _, ok := plan.packages[r]; !ok && r.sha256 != "" {
			return nil, fmt.Errorf("theme %q: required package %q missing; omit --themes to restore with themes disabled", r.id, r.sha256)
		}
	}
	return plan, nil
}

func validThemeDigest(digest string) bool {
	return len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == ""
}

func restoreThemeContent(ctx context.Context, tx *sql.Tx, plan *themeRestorePlan) (ThemeRestoreSummary, error) {
	result := ThemeRestoreSummary{Ignored: plan.ignored}
	refs, err := readThemeReferences(ctx, tx)
	if err != nil {
		return result, err
	}
	if !slices.Equal(refs, plan.refs) {
		return result, errors.New("config themes changed during restore preflight")
	}
	for _, table := range []string{"theme_file", "theme_package"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM main."+table); err != nil {
			return result, err
		}
	}
	for _, r := range refs {
		content, ok := plan.packages[r]
		if !ok {
			if _, err := tx.ExecContext(ctx, "UPDATE main.theme_version SET preview='',published=0 WHERE theme_id=? AND digest=?", r.id, r.digest); err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE main.theme_selection SET current_id='',current_digest='' WHERE current_id=? AND current_digest=?`, r.id, r.digest); err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE main.theme_selection SET previous_id='',previous_digest='' WHERE previous_id=? AND previous_digest=?`, r.id, r.digest); err != nil {
				return result, err
			}
			result.Missing = appendUniqueThemeID(result.Missing, r.id)
			continue
		}
		pkg, err := theme.Parse(content)
		if err != nil {
			return result, err
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(content))
		files := make([]ThemeFile, len(pkg.Files))
		for i, f := range pkg.Files {
			files[i] = ThemeFile{Path: f.Path, Content: f.Content}
		}
		if err := putThemeContent(tx, r.id, digest, files, content); err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE main.theme_version SET digest=?,name=?,version=?,preview=?,sdk=? WHERE theme_id=? AND digest=?`, digest, pkg.Manifest.Name, pkg.Manifest.Version, pkg.Manifest.Preview, pkg.Manifest.SDK, r.id, r.digest); err != nil {
			return result, err
		}
		if err := theme.CheckExecutable(pkg.Manifest.SDK); err != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE main.theme_version SET published=0 WHERE theme_id=? AND digest=?", r.id, digest); err != nil {
				return result, err
			}
			for _, prefix := range []string{"current", "previous"} {
				if _, err := tx.ExecContext(ctx, "UPDATE main.theme_selection SET "+prefix+"_id='',"+prefix+"_digest='' WHERE "+prefix+"_id=? AND "+prefix+"_digest=?", r.id, digest); err != nil {
					return result, err
				}
			}
		}
		result.Restored = appendUniqueThemeID(result.Restored, r.id)
	}
	return result, validateRestoredThemeSelection(ctx, tx)
}

func appendUniqueThemeID(ids []string, id string) []string {
	if !slices.Contains(ids, id) {
		return append(ids, id)
	}
	return ids
}

func validateRestoredThemeSelection(ctx context.Context, tx *sql.Tx) error {
	var selections int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM main.theme_selection WHERE id=1").Scan(&selections); err != nil {
		return err
	}
	if selections != 1 {
		return errors.New("theme selection is missing")
	}
	for _, prefix := range []string{"current", "previous"} {
		var invalid int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.theme_selection s WHERE s.`+prefix+`_id<>'' AND NOT EXISTS(SELECT 1 FROM main.theme_version v WHERE v.theme_id=s.`+prefix+`_id AND v.digest=s.`+prefix+`_digest AND v.sdk=? AND v.published=1)`, theme.SDKVersion).Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return errors.New("theme selection references an unavailable version")
		}
	}
	return nil
}
