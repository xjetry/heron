package backup

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/xjetry/heron/internal/hub/store"
)

// 这里只上传快照冻结的原包清单，不按当前主题集合清理远端：已删除或替换的包仍可能被历史快照引用。
func (m *Manager) syncThemes(ctx context.Context, cfg store.BackupSettings, client objectStore, packages []store.SnapshotThemePackage) (string, int, string) {
	prefix := strings.TrimRight(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	prefix += "theme/sha256/"
	listCtx, cancelList := context.WithTimeout(ctx, retentionBudget)
	defer cancelList()
	var firstCategory, firstDetail string
	var firstCode int
	record := func(stage string, err error) {
		if firstCategory == "" {
			firstCategory, firstCode, firstDetail = failure(stage, err)
		}
	}
	for _, entry := range packages {
		key := prefix + entry.SHA256 + ".zip"
		// 按完整键查询，不枚举不会随配置保留策略删除的整个历史原包集合。
		objects, err := client.ListObjectsV2(listCtx, key, maxListedObjects)
		if err != nil {
			record("theme_list", err)
			continue
		}
		exists := false
		for _, object := range objects {
			if object.Key == key {
				exists = true
			}
		}
		if !exists {
			f, err := os.Open(entry.Path)
			if err != nil {
				record("theme_read", err)
				continue
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				record("theme_read", err)
				continue
			}
			uploadCtx, cancelUpload := context.WithTimeout(ctx, uploadBudget(cfg, "config", info.Size()))
			err = client.PutObject(uploadCtx, key, f)
			cancelUpload()
			if err = errors.Join(err, f.Close()); err != nil {
				record("theme_upload", err)
				continue
			}
		}
		if err := m.st.MarkThemeUploaded(ctx, entry.ID, entry.SHA256, entry.Revision); err != nil {
			record("theme_record", err)
		}
	}
	return firstCategory, firstCode, firstDetail
}
