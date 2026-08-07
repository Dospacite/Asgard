package importer

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxZIPCompressed = 100 << 20
	MaxZIPExpanded   = 1 << 30
	MaxZIPFiles      = 20_000
	MaxZIPFile       = 250 << 20
)

func ExtractZIP(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.Size() > MaxZIPCompressed {
		return errors.New("ZIP exceeds the 100 MiB upload limit")
	}
	reader, err := zip.OpenReader(source)
	if err != nil {
		return fmt.Errorf("open ZIP: %w", err)
	}
	defer reader.Close()
	if len(reader.File) > MaxZIPFiles {
		return errors.New("ZIP contains more than 20,000 entries")
	}
	cleanTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	var expanded uint64
	for _, file := range reader.File {
		if file.UncompressedSize64 > MaxZIPFile {
			return fmt.Errorf("ZIP entry %q exceeds 250 MiB", file.Name)
		}
		expanded += file.UncompressedSize64
		if expanded > MaxZIPExpanded {
			return errors.New("ZIP expands beyond 1 GiB")
		}
		if file.Mode()&os.ModeSymlink != 0 || file.Mode()&os.ModeDevice != 0 || file.Mode()&os.ModeNamedPipe != 0 || file.Mode()&os.ModeSocket != 0 {
			return fmt.Errorf("ZIP entry %q has an unsafe file type", file.Name)
		}
		name := filepath.Clean(filepath.FromSlash(file.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("ZIP entry %q escapes the project directory", file.Name)
		}
		destination := filepath.Join(cleanTarget, name)
		abs, err := filepath.Abs(destination)
		if err != nil {
			return err
		}
		if abs != cleanTarget && !strings.HasPrefix(abs, cleanTarget+string(filepath.Separator)) {
			return fmt.Errorf("ZIP entry %q escapes the project directory", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(abs, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			src.Close()
			return err
		}
		written, copyErr := io.CopyN(dst, src, int64(file.UncompressedSize64)+1)
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil && copyErr != io.EOF {
			return copyErr
		}
		if written > int64(file.UncompressedSize64) {
			return fmt.Errorf("ZIP entry %q size mismatch", file.Name)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func FindCompose(root string) (string, error) {
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && !info.IsDir() {
			return name, nil
		}
	}
	entries, err := os.ReadDir(root)
	if err == nil && len(entries) == 1 && entries[0].IsDir() {
		nested := filepath.Join(root, entries[0].Name())
		for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
			if info, err := os.Stat(filepath.Join(nested, name)); err == nil && !info.IsDir() {
				return filepath.Join(entries[0].Name(), name), nil
			}
		}
	}
	return "", errors.New("no compose.yaml, compose.yml, docker-compose.yaml, or docker-compose.yml found at the archive root")
}
