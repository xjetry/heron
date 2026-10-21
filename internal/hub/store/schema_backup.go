package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// 迁移前备份：migrate 取得写锁（BEGIN IMMEDIATE）、在锁内由 schemaAdmission 判定为 schemaMigrate 之后、迁移事务写入
// 任何东西之前，经第二条连接把库做成一份一致快照放在库旁（见 migrate）。
//
// 前提：同一时刻只有一个 heron 进程在这里，由 migrate 持有的 BEGIN IMMEDIATE 写锁保证——锁在本文件的全部步骤期间
// 一直持有。下面取 seq（扫描取最大值加一）与清理遗留临时文件都依赖它：两个进程同时备份会取到同一个 seq、发布时
// EEXIST，或把对方正在写的临时文件当遗留删掉。
//
// 命名：正式名 <库文件名>.before-schema-<旧 user_version>.<seq>，seq 是持久化的递增序号——扫描同目录、同库文件名的
// 现存正式名取最大值加一，顺序只由 seq 决定，文件 mtime（墙钟）只作展示，回拨时钟不改变谁新谁旧。临时名是正式名加
// .tmp-<pid>-<8 位随机十六进制>，不参与取 seq：被 SIGKILL 或断电打断的临时文件不会让下一次启动撞上同一个临时名。
//
// 持久化顺序（任一步失败都不迁移，Open 返回点名路径与原因的错误）：独占创建临时文件 → VACUUM INTO → 临时文件 fsync →
// link 到正式名 → 删除临时名 → 父目录 fsync → 按 seq 只留最大的两份 → 才在同一段写锁里执行迁移步骤。
//   - 发布用 link 而不是 rename：正式名已存在时 link 报 EEXIST，从不覆盖已有备份；rename 会静默替换。
//   - 已有备份在新备份发布并同步目录之前一个都不动：新备份任何一步失败，旧备份的字节原样留着。
//   - 清理放在迁移之前：迁移是单个事务（migrate），失败不改库，hub 被 Restart=always 反复拉起时每次备份的都是同一个
//     原状，留两份就不会丢掉升级前的状态，也不会每次重启多出一份整库大小的文件。
//   - 临时文件按定义都不完整：只有 link 成功才算发布，正式名不存在就没有成功过；持锁者是唯一在写备份的 heron 进程，
//     别的临时文件都是被打断的进程留下的。所以开始备份前先删掉本库的全部遗留临时文件，删不掉只记警告——它们不参与
//     seq，不挡本次备份。
//
// 只认常规文件：清理与列举都先 Lstat，符号链接与其他类型跳过并记日志，不跟随链接删除或统计别处的文件。

const schemaBackupInfix = ".before-schema-"

// keepSchemaBackups 是保留的迁移前备份份数（按 seq 取最大的几份）。
const keepSchemaBackups = 2

// backupFiles 是迁移前备份对文件系统与库的全部操作。生产实现 osBackupFiles 直接调 os 与 SQLite；测试经 withBackupFiles
// 替换其中一步注入失败，每一步失败后的处理（不迁移、清掉临时文件、旧备份不动）因此能逐步验证。库的内容只经 Vacuum 读出，
// 其余操作的对象都是备份文件与库所在的目录：备份期间 migrate 持着写锁，打开库的主文件或 -shm 再关掉，会放掉本进程在
// 那个文件上的锁（见包注释）。
type backupFiles interface {
	ReadDir(dir string) ([]fs.DirEntry, error)
	Lstat(path string) (fs.FileInfo, error)
	// CreateExclusive 以 O_CREATE|O_EXCL、0600 建出空文件：VACUUM INTO 只接受不存在或为空的目标，权限在创建时就定下。
	CreateExclusive(path string) error
	Vacuum(ctx context.Context, db *sql.DB, path string) error
	SyncFile(path string) error
	Link(oldname, newname string) error
	Remove(path string) error
	SyncDir(dir string) error
}

type osBackupFiles struct{}

func (osBackupFiles) ReadDir(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }
func (osBackupFiles) Lstat(path string) (fs.FileInfo, error)    { return os.Lstat(path) }
func (osBackupFiles) Link(oldname, newname string) error        { return os.Link(oldname, newname) }
func (osBackupFiles) Remove(path string) error                  { return os.Remove(path) }

func (osBackupFiles) CreateExclusive(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Vacuum 的目标路径经参数绑定，不拼进 SQL：库路径可以含引号。WAL 模式下 VACUUM INTO 读的是一个读事务的快照，
// 含已提交但尚未检查点进主文件的页。
func (osBackupFiles) Vacuum(ctx context.Context, db *sql.DB, path string) error {
	_, err := db.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}

func (osBackupFiles) SyncFile(path string) error { return syncPath(path, os.O_RDWR) }
func (osBackupFiles) SyncDir(dir string) error   { return syncPath(dir, os.O_RDONLY) }

func syncPath(path string, flag int) error {
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// withBackupFiles 替换迁移前备份的文件操作，只供本包测试注入失败。
func withBackupFiles(f backupFiles) OpenOption { return func(c *openConfig) { c.backups = f } }

// schemaBackupName 是一个正式备份名解析出的旧 schema 版本与 seq。
type schemaBackupName struct {
	version int
	seq     uint64
}

// parseCanonicalUint 只认十进制规范写法（无符号、无前导零）：生成时总是这样写，其他写法不是本程序产生的名字。
func parseCanonicalUint(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != s {
		return 0, false
	}
	return n, true
}

// parseSchemaBackup 解析 rest = 名字去掉 "<库文件名>.before-schema-" 之后的部分：正式名是 "<v>.<seq>"，
// 临时名（及 VACUUM INTO 被打断时可能留下的同前缀附属文件）是 "<v>.<seq>.tmp-<任意>"。
func parseSchemaBackup(rest string) (name schemaBackupName, temporary, ok bool) {
	parts := strings.SplitN(rest, ".", 3)
	if len(parts) < 2 {
		return name, false, false
	}
	v, okV := parseCanonicalUint(parts[0])
	seq, okS := parseCanonicalUint(parts[1])
	if !okV || !okS || v > math.MaxInt32 || seq == 0 {
		return name, false, false
	}
	name = schemaBackupName{version: int(v), seq: seq}
	if len(parts) == 2 {
		return name, false, true
	}
	if strings.HasPrefix(parts[2], "tmp-") && len(parts[2]) > len("tmp-") {
		return name, true, true
	}
	return name, false, false
}

type schemaBackupEntry struct {
	schemaBackupName
	path      string
	temporary bool
}

// scanSchemaBackups 列出 dbPath 同目录、同库文件名的全部正式名与临时名（任何文件类型），由调用方决定是否只认常规文件。
func scanSchemaBackups(files backupFiles, dbPath string) ([]schemaBackupEntry, error) {
	dir := filepath.Dir(dbPath)
	prefix := filepath.Base(dbPath) + schemaBackupInfix
	entries, err := files.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []schemaBackupEntry
	for _, e := range entries {
		rest, found := strings.CutPrefix(e.Name(), prefix)
		if !found {
			continue
		}
		name, temporary, ok := parseSchemaBackup(rest)
		if !ok {
			continue
		}
		out = append(out, schemaBackupEntry{schemaBackupName: name, path: filepath.Join(dir, e.Name()), temporary: temporary})
	}
	return out, nil
}

// removeRegular 只删 Lstat 为常规文件的路径；符号链接与其他类型不删，返回 false 由调用方记日志。
func removeRegular(files backupFiles, path string) (removed bool, err error) {
	info, err := files.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	return true, files.Remove(path)
}

// backupBeforeMigration 把 db 备份成 dbPath 旁的一份正式备份，并按 seq 只留最大的 keepSchemaBackups 份。调用方必须
// 持着这个库的写锁、且持锁事务尚未写入，db 是另一条连接（见 migrate 的 backupUnderLock）。返回 nil 才能迁移；顺序与
// 每一步的理由见本文件开头。
func backupBeforeMigration(ctx context.Context, db *sql.DB, dbPath string, version int, files backupFiles, log *slog.Logger) error {
	dir := filepath.Dir(dbPath)
	existing, err := scanSchemaBackups(files, dbPath)
	if err != nil {
		return fmt.Errorf("scan %s for pre-migration backups: %w", dir, err)
	}
	var maxSeq uint64
	for _, e := range existing {
		if e.temporary {
			removeStaleBackupTemp(files, e.path, log)
			continue
		}
		// seq 取全部正式名（不论文件类型）的最大值：本次的正式名因此与任何已存在的名字都不同，link 的 EEXIST 只会
		// 出现在扫描之后又被别人占用的情形。
		maxSeq = max(maxSeq, e.seq)
	}
	if maxSeq == math.MaxUint64 {
		return fmt.Errorf("pre-migration backup sequence in %s is exhausted", dir)
	}
	seq := maxSeq + 1
	formal := filepath.Join(dir, fmt.Sprintf("%s%s%d.%d", filepath.Base(dbPath), schemaBackupInfix, version, seq))
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.tmp-%d-%s", formal, os.Getpid(), hex.EncodeToString(suffix[:]))
	if err := files.CreateExclusive(temp); err != nil {
		return fmt.Errorf("create pre-migration backup %s: %w", temp, err)
	}
	if err := writeBackup(ctx, db, files, temp, formal); err != nil {
		// link 之前的任何失败都只留下临时文件，删掉它；正式名此时不存在或不属于本次。
		if rmErr := files.Remove(temp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove incomplete backup %s: %w", temp, rmErr))
		}
		return err
	}
	// 从这里起正式名已发布：它是一份完整、已 fsync 的快照，下面任何失败都保留它，不迁移；下次启动按 seq + 1 另建，
	// 不复用它（那时库可能已被别的写者改过）。
	if err := files.Remove(temp); err != nil {
		return fmt.Errorf("pre-migration backup %s was published but its temporary name %s was not removed: %w", formal, temp, err)
	}
	if err := files.SyncDir(dir); err != nil {
		return fmt.Errorf("pre-migration backup %s was written but its directory was not synced: %w", formal, err)
	}
	var size int64
	if info, err := files.Lstat(formal); err == nil {
		size = info.Size()
	} else {
		return fmt.Errorf("stat pre-migration backup %s: %w", formal, err)
	}
	log.Info("database backed up before migration", "path", formal, "schema_version", version, "bytes", size, "seq", seq)
	pruneSchemaBackups(files, dbPath, log)
	return nil
}

// writeBackup 写临时文件、fsync，再 link 到正式名。返回错误时正式名不是本次写出的（link 未成功），调用方删临时文件。
func writeBackup(ctx context.Context, db *sql.DB, files backupFiles, temp, formal string) error {
	if err := files.Vacuum(ctx, db, temp); err != nil {
		return fmt.Errorf("write pre-migration backup %s: %w", temp, err)
	}
	if err := files.SyncFile(temp); err != nil {
		return fmt.Errorf("sync pre-migration backup %s: %w", temp, err)
	}
	if err := files.Link(temp, formal); err != nil {
		return fmt.Errorf("publish pre-migration backup %s: %w", formal, err)
	}
	return nil
}

func removeStaleBackupTemp(files backupFiles, path string, log *slog.Logger) {
	removed, err := removeRegular(files, path)
	switch {
	case err != nil:
		log.Warn("could not remove an incomplete pre-migration backup", "path", path, "err", err)
	case !removed:
		log.Warn("skipped an incomplete pre-migration backup that is not a regular file", "path", path)
	default:
		log.Info("removed an incomplete pre-migration backup", "path", path)
	}
}

// pruneSchemaBackups 按 seq 只留最大的 keepSchemaBackups 份常规文件：符号链接与其他类型既不算进保留的份数，也不删除。
// 清理失败只记警告：多留一个文件不影响库，启动继续，下次备份之后再清。
func pruneSchemaBackups(files backupFiles, dbPath string, log *slog.Logger) {
	all, err := scanSchemaBackups(files, dbPath)
	if err != nil {
		log.Warn("could not list pre-migration backups for cleanup", "dir", filepath.Dir(dbPath), "err", err)
		return
	}
	var backups []schemaBackupEntry
	for _, e := range all {
		if e.temporary {
			continue
		}
		info, err := files.Lstat(e.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			log.Warn("could not inspect a pre-migration backup for cleanup", "path", e.path, "err", err)
		case !info.Mode().IsRegular():
			log.Warn("skipped a pre-migration backup name that is not a regular file", "path", e.path)
		default:
			backups = append(backups, e)
		}
	}
	slices.SortFunc(backups, func(a, b schemaBackupEntry) int { return compareUint(b.seq, a.seq) })
	for _, e := range backups[min(keepSchemaBackups, len(backups)):] {
		if err := files.Remove(e.path); err != nil {
			log.Warn("could not remove an older pre-migration backup", "path", e.path, "err", err)
			continue
		}
		log.Info("removed an older pre-migration backup", "path", e.path, "schema_version", e.version, "seq", e.seq)
	}
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// SchemaBackup 是库旁的一份迁移前备份。ModifiedAt 是文件 mtime 的 Unix 秒，只作展示；新旧只看 Seq。
type SchemaBackup struct {
	Path          string
	SchemaVersion int
	Seq           uint64
	Bytes         int64
	ModifiedAt    int64
}

// listSchemaBackups 列出库旁的正式备份（只认常规文件），按 seq 从新到旧。
func listSchemaBackups(files backupFiles, dbPath string) ([]SchemaBackup, error) {
	all, err := scanSchemaBackups(files, dbPath)
	if err != nil {
		return nil, err
	}
	var out []SchemaBackup
	for _, e := range all {
		if e.temporary {
			continue
		}
		// 只认常规文件以 Lstat 为准：大小与 mtime 也取自这次 Lstat，判定与读数出自同一次观测。
		info, err := files.Lstat(e.path)
		if errors.Is(err, fs.ErrNotExist) {
			// 列目录之后被清理删掉：它已不是一份备份。
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, SchemaBackup{Path: e.path, SchemaVersion: e.version, Seq: e.seq, Bytes: info.Size(), ModifiedAt: info.ModTime().Unix()})
	}
	slices.SortFunc(out, func(a, b SchemaBackup) int { return compareUint(b.Seq, a.Seq) })
	return out, nil
}
