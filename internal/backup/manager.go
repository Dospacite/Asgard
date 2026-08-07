package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/store"
)

type Manager struct {
	Store       *store.Store
	Docker      *dockerx.Engine
	BackupsDir  string
	DataVolume  string
	HelperImage string
}
type payload struct {
	BackupID string `json:"backupId"`
	VolumeID string `json:"volumeId"`
}
type volumeInfo struct{ ID, ProjectID, Name, ProjectSlug string }

func (m *Manager) HandleCreate(ctx context.Context, op store.Operation) error {
	var p payload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return err
	}
	return m.create(ctx, op, p, false)
}
func (m *Manager) HandleRestore(ctx context.Context, op store.Operation) error {
	var p payload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return err
	}
	if p.BackupID == "" {
		return errors.New("backupId is required")
	}
	var sourcePath, volumeID, status string
	if err := m.Store.DB.QueryRowContext(ctx, `SELECT path,volume_id,status FROM backups WHERE id=?`, p.BackupID).Scan(&sourcePath, &volumeID, &status); err != nil {
		return err
	}
	if status != "succeeded" {
		return errors.New("only a successful backup can be restored")
	}
	p.VolumeID = volumeID
	info, err := m.volume(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	preID := "pre-restore-" + op.ID
	_, err = m.Store.DB.ExecContext(ctx, `INSERT INTO backups(id,project_id,volume_id,operation_id,kind,status,created_at) VALUES(?,?,?,?, 'pre-restore','running',?)`, preID, info.ProjectID, info.ID, op.ID, store.Now())
	if err != nil {
		return err
	}
	if err = m.create(ctx, op, pWith(p, preID), true); err != nil {
		return fmt.Errorf("pre-restore backup: %w", err)
	}
	stopped := m.stopProject(ctx, info.ProjectID)
	defer m.restartProject(context.WithoutCancel(ctx), stopped)
	_ = m.Store.ProgressOperation(ctx, op.ID, 55, "Restoring volume snapshot")
	relative, err := filepath.Rel(m.BackupsDir, sourcePath)
	if err != nil || strings.HasPrefix(relative, "..") {
		return errors.New("backup path is outside the managed backup directory")
	}
	script := fmt.Sprintf("set -eu; find /volume -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf %s -C /volume", shellQuote(filepath.ToSlash(filepath.Join("/asgard-data/backups", relative))))
	mounts := []mount.Mount{{Type: mount.TypeVolume, Source: info.Name, Target: "/volume"}, {Type: mount.TypeVolume, Source: m.DataVolume, Target: "/asgard-data", ReadOnly: true}}
	if err = m.Docker.RunHelper(ctx, m.HelperImage, "asgard-restore-"+op.ID[:8], script, mounts, func(line string) { _ = m.Store.LogOperation(ctx, op.ID, "info", line) }); err != nil {
		return err
	}
	_ = m.Store.ProgressOperation(ctx, op.ID, 90, "Volume restored; restarting services")
	return nil
}

func pWith(p payload, id string) payload { p.BackupID = id; return p }
func (m *Manager) create(ctx context.Context, op store.Operation, p payload, existing bool) error {
	info, err := m.volume(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	if p.BackupID == "" {
		return errors.New("backupId is required")
	}
	if !existing {
		if _, err = m.Store.DB.ExecContext(ctx, `UPDATE backups SET status='running' WHERE id=?`, p.BackupID); err != nil {
			return err
		}
	}
	stopped := m.stopProject(ctx, info.ProjectID)
	defer m.restartProject(context.WithoutCancel(ctx), stopped)
	_ = m.Store.ProgressOperation(ctx, op.ID, 20, "Services stopped for a consistent snapshot")
	dir := filepath.Join(m.BackupsDir, info.ProjectSlug)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	filename := fmt.Sprintf("%s-%s.tar.gz", safe(info.Name), time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, filename)
	relative, err := filepath.Rel(m.BackupsDir, path)
	if err != nil {
		return err
	}
	script := fmt.Sprintf("set -eu; mkdir -p %s; tar -czf %s -C /volume .", shellQuote(filepath.ToSlash(filepath.Dir(filepath.Join("/asgard-data/backups", relative)))), shellQuote(filepath.ToSlash(filepath.Join("/asgard-data/backups", relative))))
	mounts := []mount.Mount{{Type: mount.TypeVolume, Source: info.Name, Target: "/volume", ReadOnly: true}, {Type: mount.TypeVolume, Source: m.DataVolume, Target: "/asgard-data"}}
	if err = m.Docker.RunHelper(ctx, m.HelperImage, "asgard-backup-"+op.ID[:8], script, mounts, func(line string) { _ = m.Store.LogOperation(ctx, op.ID, "info", line) }); err != nil {
		_, _ = m.Store.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE backups SET status='failed',error=?,completed_at=? WHERE id=?`, err.Error(), store.Now(), p.BackupID)
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	file.Close()
	if err != nil {
		return err
	}
	_, err = m.Store.DB.ExecContext(ctx, `UPDATE backups SET status='succeeded',path=?,size_bytes=?,sha256=?,completed_at=? WHERE id=?`, path, size, hex.EncodeToString(hash.Sum(nil)), store.Now(), p.BackupID)
	_ = m.Store.ProgressOperation(ctx, op.ID, 90, "Snapshot verified; restarting services")
	return err
}

func (m *Manager) volume(ctx context.Context, id string) (volumeInfo, error) {
	var item volumeInfo
	err := m.Store.DB.QueryRowContext(ctx, `SELECT v.id,v.project_id,v.name,p.slug FROM volumes v JOIN projects p ON p.id=v.project_id WHERE v.id=?`, id).Scan(&item.ID, &item.ProjectID, &item.Name, &item.ProjectSlug)
	return item, err
}
func (m *Manager) stopProject(ctx context.Context, projectID string) []string {
	runtimes, err := m.Store.ActiveRuntimes(ctx, projectID)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, rt := range runtimes {
		if rt.State == "running" {
			if m.Docker.Action(ctx, rt.DockerID, "stop") == nil {
				out = append(out, rt.DockerID)
			}
		}
	}
	return out
}
func (m *Manager) restartProject(ctx context.Context, ids []string) {
	for i := len(ids) - 1; i >= 0; i-- {
		_ = m.Docker.Action(ctx, ids[i], "start")
	}
}
func safe(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func IsNotFound(err error) bool      { return errors.Is(err, sql.ErrNoRows) }
