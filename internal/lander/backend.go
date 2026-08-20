package lander

import (
	"context"
	"os"
	"path"
	"path/filepath"
)

// Backend 抽象落地所需的最小文件操作集。
//
// 存在两种实现，因为两条网盘线路的可写能力完全不同：
//
//	MountBackend — rclone WebDAV 挂载（夸克/百度）。挂载支持 rename，
//	               但不支持新建文件写入，所以落地只能 os.Rename，绝不复制。
//	AlistBackend — Alist HTTP API（光鸭）。光鸭的 CloudDrive2 CloudFS 挂载
//	               是只读的，mkdir/rename 全部失败；ADR-001 曾因此判定光鸭
//	               无法落地。实测证明走 Alist 的 GuangYaPan 驱动可以
//	               mkdir/rename/move，且无需任何 FUSE 挂载与缓存。
type Backend interface {
	// Name 后端名称（日志用）
	Name() string

	// Walk 递归遍历 root 下所有文件，对每个文件回调（不含目录本身）。
	// rel 是相对 root 的路径，统一用 "/" 分隔。
	Walk(ctx context.Context, root string, fn func(rel string) error) error

	// MkdirAll 递归创建目录（已存在时不报错）
	MkdirAll(ctx context.Context, dir string) error

	// Exists 判断路径是否存在
	Exists(ctx context.Context, p string) (bool, error)

	// Rename 把 src 改名/移动到 dst（同盘内，绝不跨盘复制）
	Rename(ctx context.Context, src, dst string) error
}

// ============================================================
// MountBackend：本地文件系统 / rclone 挂载
// ============================================================

// MountBackend 直接操作本地文件系统（rclone 挂载点也算本地路径）。
type MountBackend struct{}

func (MountBackend) Name() string { return "挂载" }

func (MountBackend) Walk(ctx context.Context, root string, fn func(rel string) error) error {
	return filepath.Walk(root, func(full string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return nil // 单个文件算不出相对路径就跳过，不中断整批
		}
		return fn(filepath.ToSlash(rel))
	})
}

func (MountBackend) MkdirAll(_ context.Context, dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func (MountBackend) Exists(_ context.Context, p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (MountBackend) Rename(_ context.Context, src, dst string) error {
	return os.Rename(src, dst)
}

// joinPath 拼接虚拟路径（始终用 "/"，不受宿主 OS 影响）
func joinPath(parts ...string) string {
	return path.Join(parts...)
}
