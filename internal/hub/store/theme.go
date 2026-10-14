package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/xjetry/heron/internal/hub/theme"
)

// Theme 表示一个不可变安装产物；版本字符串用于展示，Digest 才是包身份。
type Theme struct {
	ID, Name, Version, Preview   string
	Digest                       string
	SDK                          int
	Repository, Release, Asset   string
	UploadedAt                   time.Time
	Enabled, Previous, Published bool
}

type ThemeFile struct {
	Path    string
	Content []byte
}

const MaxThemeVersions = 3

var ErrThemeLimit = errors.New("theme limit reached")
var ErrThemeVersionLimit = errors.New("theme version limit reached")
var ErrThemeInUse = errors.New("theme version is current or previous")
var ErrThemeContentMissing = errors.New("theme has no files; upload the original package again")

// 所有在线元数据、选择与文件写者经此入口；先提交后递增，读者不会将旧内容标成新代数。
// 迁移发生在 Open 返回前，离线恢复要求 hub 停机，原包备份状态不影响托管内容。
func (s *Store) writeTheme(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := s.write(ctx, fn); err != nil {
		return err
	}
	s.themeGen.Add(1)
	return nil
}

func (s *Store) ThemeGeneration() uint64 { return s.themeGen.Load() }

// PutTheme 只安装产物，不更改全站选择。相同摘要重复安装返回已有元数据，不覆盖不可变文件。
// 主题数、版本数及缺包占位的清理与插入处于同一写事务，并发安装不能越过上限。
func (s *Store) PutTheme(ctx context.Context, t Theme, files []ThemeFile, content []byte, mustExist bool, limit int) (Theme, error) {
	if err := theme.CheckExecutable(t.SDK); err != nil {
		return Theme{}, err
	}
	if len(content) == 0 || len(files) == 0 {
		return Theme{}, ErrThemeContentMissing
	}
	t.Digest = fmt.Sprintf("%x", sha256.Sum256(content))
	var out Theme
	err := s.writeTheme(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM theme WHERE id=?)", t.ID).Scan(&exists); err != nil {
			return err
		}
		if mustExist && !exists {
			return ErrNotFound
		}
		if !exists {
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM theme").Scan(&n); err != nil {
				return err
			}
			if limit <= 0 || n >= min(limit, theme.MaxThemes) {
				return ErrThemeLimit
			}
			if _, err := tx.Exec("INSERT INTO theme(id) VALUES (?)", t.ID); err != nil {
				return err
			}
		}
		previous, err := scanTheme(tx.QueryRow(themeSelect+" WHERE v.theme_id=? AND v.digest=?", t.ID, t.Digest))
		if err == nil {
			var hasPackage bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM theme_package WHERE theme_id=? AND digest=?)", t.ID, t.Digest).Scan(&hasPackage); err != nil {
				return err
			}
			// 无原包恢复保留版本身份；补回同一份原包时恢复文件，仍然不能改写全站选择。
			if !hasPackage {
				if _, err := tx.Exec("DELETE FROM theme_file WHERE theme_id=? AND digest=?", t.ID, t.Digest); err != nil {
					return err
				}
				if err := putThemeContent(tx, t.ID, t.Digest, files, content); err != nil {
					return err
				}
				if _, err := tx.Exec("UPDATE theme_version SET preview=? WHERE theme_id=? AND digest=?", t.Preview, t.ID, t.Digest); err != nil {
					return err
				}
				previous.Preview = t.Preview
			}
			out = previous
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		// 空摘要只表示迁移或恢复留下的缺原包归档，不能执行，也不是可保留的安装产物。
		if _, err := tx.Exec("DELETE FROM theme_file WHERE theme_id=? AND digest=''", t.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM theme_version WHERE theme_id=? AND digest=''", t.ID); err != nil {
			return err
		}
		var versions int
		if err := tx.QueryRow("SELECT COUNT(*) FROM theme_version WHERE theme_id=?", t.ID).Scan(&versions); err != nil {
			return err
		}
		if versions >= MaxThemeVersions {
			return ErrThemeVersionLimit
		}
		if _, err := tx.Exec(`INSERT INTO theme_version
			(theme_id,digest,name,version,preview,uploaded_at,sdk,published,repository,release,asset)
			VALUES (?,?,?,?,?,?,?,0,?,?,?)`, t.ID, t.Digest, t.Name, t.Version, t.Preview, t.UploadedAt.Unix(), t.SDK, t.Repository, t.Release, t.Asset); err != nil {
			return err
		}
		if err := putThemeContent(tx, t.ID, t.Digest, files, content); err != nil {
			return err
		}
		out, err = scanTheme(tx.QueryRow(themeSelect+" WHERE v.theme_id=? AND v.digest=?", t.ID, t.Digest))
		return err
	})
	if err != nil {
		return Theme{}, err
	}
	s.wakeThemeBackup()
	return out, nil
}

// 原包、文件与版本元数据由调用方放在同一事务，任一失败都不能留下半个产物。
func putThemeContent(tx *sql.Tx, id, digest string, files []ThemeFile, content []byte) error {
	if len(content) == 0 || fmt.Sprintf("%x", sha256.Sum256(content)) != digest {
		return errors.New("theme package digest mismatch")
	}
	var revision int64
	for revision == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		revision = int64(binary.LittleEndian.Uint64(b[:]) & (1<<63 - 1))
	}
	if _, err := tx.Exec("INSERT INTO theme_package(theme_id,digest,content,revision,uploaded) VALUES (?,?,?,?,0)", id, digest, content, revision); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec("INSERT INTO theme_file(theme_id,digest,path,content) VALUES (?,?,?,?)", id, digest, f.Path, f.Content); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ThemeChanges() <-chan struct{} { return s.themeChanges }
func (s *Store) wakeThemeBackup() {
	select {
	case s.themeChanges <- struct{}{}:
	default:
	}
}

type ThemeBackupEntry struct {
	ID, Digest           string
	Revision             int64
	Uploaded, HasPackage bool
}

func (s *Store) ThemeBackupEntries(ctx context.Context) ([]ThemeBackupEntry, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT v.theme_id,v.digest,COALESCE(p.revision,0),COALESCE(p.uploaded,0),p.theme_id IS NOT NULL
		FROM theme_version v LEFT JOIN theme_package p ON p.theme_id=v.theme_id AND p.digest=v.digest ORDER BY v.theme_id,v.digest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThemeBackupEntry
	for rows.Next() {
		var e ThemeBackupEntry
		if err := rows.Scan(&e.ID, &e.Digest, &e.Revision, &e.Uploaded, &e.HasPackage); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ThemeBackupContent(ctx context.Context, id, digest string, revision int64) ([]byte, error) {
	var content []byte
	err := s.r.QueryRowContext(ctx, "SELECT content FROM theme_package WHERE theme_id=? AND digest=? AND revision=?", id, digest, revision).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return content, err
}

// ThemePackage 只读原始归档；执行准入由启用和文件托管入口分别检查。
func (s *Store) ThemePackage(ctx context.Context, id, digest string) ([]byte, error) {
	var content []byte
	err := s.r.QueryRowContext(ctx, "SELECT content FROM theme_package WHERE theme_id=? AND digest=?", id, digest).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return content, err
}

func (s *Store) ThemesWithoutPackage(ctx context.Context) ([]string, error) {
	entries, err := s.ThemeBackupEntries(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.HasPackage {
			ids = appendUniqueThemeID(ids, e.ID)
		}
	}
	return ids, nil
}

// 上传确认必须同时命中产物摘要和本次落库标识，删除重装不能继承旧上传任务的成功状态。
func (s *Store) MarkThemeUploaded(ctx context.Context, id, digest string, revision int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE theme_package SET uploaded=1 WHERE theme_id=? AND digest=? AND revision=?", id, digest, revision)
		return err
	})
}

const themeSelect = `SELECT v.theme_id,v.digest,v.name,v.version,v.preview,v.uploaded_at,v.sdk,v.published,v.repository,v.release,v.asset,
	(v.theme_id=s.current_id AND v.digest=s.current_digest),(v.theme_id=s.previous_id AND v.digest=s.previous_digest)
	FROM theme_version v CROSS JOIN theme_selection s`

func scanTheme(row interface{ Scan(...any) error }) (Theme, error) {
	var t Theme
	var at int64
	err := row.Scan(&t.ID, &t.Digest, &t.Name, &t.Version, &t.Preview, &at, &t.SDK, &t.Published, &t.Repository, &t.Release, &t.Asset, &t.Enabled, &t.Previous)
	if errors.Is(err, sql.ErrNoRows) {
		return Theme{}, ErrNotFound
	}
	t.UploadedAt = time.Unix(at, 0).UTC()
	return t, err
}

func (s *Store) ListThemes(ctx context.Context) ([]Theme, error) {
	rows, err := s.r.QueryContext(ctx, themeSelect+" ORDER BY v.theme_id,v.uploaded_at DESC,v.digest")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Theme
	for rows.Next() {
		t, err := scanTheme(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ThemeSelection(ctx context.Context) (current Theme, previous Theme, err error) {
	rows, err := s.r.QueryContext(ctx, themeSelect+" WHERE (v.theme_id=s.current_id AND v.digest=s.current_digest) OR (v.theme_id=s.previous_id AND v.digest=s.previous_digest)")
	if err != nil {
		return Theme{}, Theme{}, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTheme(rows)
		if err != nil {
			return Theme{}, Theme{}, err
		}
		if t.Enabled {
			current = t
		}
		if t.Previous {
			previous = t
		}
	}
	return current, previous, rows.Err()
}

// 空 id 与空 digest 同时表示内置页；缺一个参数不是放宽匹配，也不能选择缺包归档。
func (s *Store) EnableTheme(ctx context.Context, id, digest string) error {
	if (id == "") != (digest == "") {
		return ErrNotFound
	}
	return s.writeTheme(ctx, func(tx *sql.Tx) error {
		if id != "" {
			t, err := scanTheme(tx.QueryRow(themeSelect+" WHERE v.theme_id=? AND v.digest=?", id, digest))
			if err != nil {
				return err
			}
			if err := theme.CheckExecutable(t.SDK); err != nil {
				return err
			}
			var hasContent bool
			if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM theme_file WHERE theme_id=? AND digest=? AND path='index.html')", id, digest).Scan(&hasContent); err != nil {
				return err
			}
			if !hasContent {
				return ErrThemeContentMissing
			}
			if _, err := tx.Exec("UPDATE theme_version SET published=1 WHERE theme_id=? AND digest=?", id, digest); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE theme_selection SET previous_id=current_id,previous_digest=current_digest,current_id=?,current_digest=?
			WHERE id=1 AND (current_id<>? OR current_digest<>?)`, id, digest, id, digest)
		return err
	})
}

func (s *Store) DeleteThemeVersion(ctx context.Context, id, digest string) error {
	err := s.writeTheme(ctx, func(tx *sql.Tx) error {
		t, err := scanTheme(tx.QueryRow(themeSelect+" WHERE v.theme_id=? AND v.digest=?", id, digest))
		if err != nil {
			return err
		}
		if t.Enabled || t.Previous {
			return ErrThemeInUse
		}
		for _, table := range []string{"theme_file", "theme_package", "theme_version"} {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE theme_id=? AND digest=?", id, digest); err != nil {
				return err
			}
		}
		_, err = tx.Exec("DELETE FROM theme WHERE id=? AND NOT EXISTS (SELECT 1 FROM theme_version WHERE theme_id=?)", id, id)
		return err
	})
	if err == nil {
		s.wakeThemeBackup()
	}
	return err
}

// 整主题删除是显式卸载；与版本清理不同，它同时撤销该主题的全站选择。
// 当前引用被删除时回到内置页，其他主题的回滚引用不受影响。
func (s *Store) DeleteTheme(ctx context.Context, id string) error {
	err := s.writeTheme(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM theme WHERE id=?)", id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		for _, prefix := range []string{"current", "previous"} {
			if _, err := tx.Exec("UPDATE theme_selection SET "+prefix+"_id='',"+prefix+"_digest='' WHERE "+prefix+"_id=?", id); err != nil {
				return err
			}
		}
		for _, table := range []string{"theme_file", "theme_package", "theme_version"} {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE theme_id=?", id); err != nil {
				return err
			}
		}
		_, err := tx.Exec("DELETE FROM theme WHERE id=?", id)
		return err
	})
	if err == nil {
		s.wakeThemeBackup()
	}
	return err
}

func (s *Store) ThemeVersion(ctx context.Context, id, digest string) (Theme, error) {
	return scanTheme(s.r.QueryRowContext(ctx, themeSelect+" WHERE v.theme_id=? AND v.digest=?", id, digest))
}

func (s *Store) ThemePreview(ctx context.Context, id, digest string) (Theme, []byte, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Theme{}, nil, err
	}
	defer tx.Rollback()
	t, err := scanTheme(tx.QueryRowContext(ctx, themeSelect+" WHERE v.theme_id=? AND v.digest=?", id, digest))
	if err != nil {
		return Theme{}, nil, err
	}
	if err := theme.CheckExecutable(t.SDK); err != nil {
		return Theme{}, nil, err
	}
	var content []byte
	err = tx.QueryRowContext(ctx, "SELECT content FROM theme_file WHERE theme_id=? AND digest=? AND path=?", id, digest, t.Preview).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return t, content, err
}

// 读取固定摘要的元数据与文件属于同一事务，删除或切换不能拼出不同产物的视图。
// 此方法不授予公开访问权；HTTP 入口仍须检查 SDK 与公开发布或已认证预览权限。
func (s *Store) ThemeVersionFile(ctx context.Context, id, digest, path string) (Theme, []byte, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Theme{}, nil, err
	}
	defer tx.Rollback()
	t, err := scanTheme(tx.QueryRowContext(ctx, themeSelect+" WHERE v.theme_id=? AND v.digest=?", id, digest))
	if err != nil {
		return Theme{}, nil, err
	}
	var content []byte
	err = tx.QueryRowContext(ctx, "SELECT content FROM theme_file WHERE theme_id=? AND digest=? AND path=?", id, digest, path).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return t, content, err
}
