package importer

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Archive uploads share one budget regardless of container format so a tarball
// cannot expand further than a ZIP of the same upload size.
const (
	MaxArchiveCompressed = 100 << 20
	MaxArchiveExpanded   = 1 << 30
	MaxArchiveFiles      = 20_000
	MaxArchiveFile       = 250 << 20
)

// Format identifies a supported project upload container.
type Format string

const (
	FormatZIP    Format = "zip"
	FormatTar    Format = "tar"
	FormatTarGz  Format = "tar.gz"
	FormatTarBz2 Format = "tar.bz2"
	FormatTarXz  Format = "tar.xz"
	FormatTarZst Format = "tar.zst"
)

// ArchiveExtensions lists every accepted upload suffix, longest first so that
// ".tar.gz" wins over ".gz" during matching.
var ArchiveExtensions = []string{
	".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst",
	".tgz", ".tbz2", ".tbz", ".txz", ".tzst",
	".zip", ".tar",
}

var extensionFormats = map[string]Format{
	".zip":     FormatZIP,
	".tar":     FormatTar,
	".tar.gz":  FormatTarGz,
	".tgz":     FormatTarGz,
	".tar.bz2": FormatTarBz2,
	".tbz2":    FormatTarBz2,
	".tbz":     FormatTarBz2,
	".tar.xz":  FormatTarXz,
	".txz":     FormatTarXz,
	".tar.zst": FormatTarZst,
	".tzst":    FormatTarZst,
}

// HasArchiveExtension reports whether a filename carries a supported suffix.
func HasArchiveExtension(filename string) bool {
	lower := strings.ToLower(strings.TrimSpace(filename))
	for _, ext := range ArchiveExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// FormatFromName resolves a format from a filename suffix alone.
func FormatFromName(filename string) (Format, bool) {
	lower := strings.ToLower(strings.TrimSpace(filename))
	for _, ext := range ArchiveExtensions {
		if strings.HasSuffix(lower, ext) {
			return extensionFormats[ext], true
		}
	}
	return "", false
}

// DetectFormat inspects the file's own bytes and only falls back to the
// uploaded name when the content is inconclusive. Content wins so a mislabeled
// upload is still extracted with the correct reader instead of being rejected.
func DetectFormat(path, filename string) (Format, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	header := make([]byte, 512)
	read, err := io.ReadFull(file, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	header = header[:read]
	switch {
	case bytes.HasPrefix(header, []byte("PK\x03\x04")), bytes.HasPrefix(header, []byte("PK\x05\x06")), bytes.HasPrefix(header, []byte("PK\x07\x08")):
		return FormatZIP, nil
	case bytes.HasPrefix(header, []byte{0x1f, 0x8b}):
		return FormatTarGz, nil
	case bytes.HasPrefix(header, []byte("BZh")):
		return FormatTarBz2, nil
	case bytes.HasPrefix(header, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		return FormatTarXz, nil
	case bytes.HasPrefix(header, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return FormatTarZst, nil
	case len(header) >= 262 && bytes.HasPrefix(header[257:], []byte("ustar")):
		return FormatTar, nil
	}
	if format, ok := FormatFromName(filename); ok {
		return format, nil
	}
	return "", errors.New("unrecognized archive; upload a .zip, .tar, .tar.gz, .tar.bz2, .tar.xz, or .tar.zst file")
}

// ExtractArchive safely expands any supported archive into target.
func ExtractArchive(source, target, filename string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.Size() > MaxArchiveCompressed {
		return errors.New("archive exceeds the 100 MiB upload limit")
	}
	format, err := DetectFormat(source, filename)
	if err != nil {
		return err
	}
	if format == FormatZIP {
		return ExtractZIP(source, target)
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	stream, closer, err := decompress(format, bufio.NewReaderSize(file, 1<<20))
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	return extractTar(tar.NewReader(stream), target)
}

func decompress(format Format, reader io.Reader) (io.Reader, func(), error) {
	switch format {
	case FormatTar:
		return reader, nil, nil
	case FormatTarGz:
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, nil, fmt.Errorf("open gzip archive: %w", err)
		}
		return gz, func() { _ = gz.Close() }, nil
	case FormatTarBz2:
		return bzip2.NewReader(reader), nil, nil
	case FormatTarXz:
		xzReader, err := xz.NewReader(reader)
		if err != nil {
			return nil, nil, fmt.Errorf("open xz archive: %w", err)
		}
		return xzReader, nil, nil
	case FormatTarZst:
		zstReader, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return nil, nil, fmt.Errorf("open zstd archive: %w", err)
		}
		return zstReader.IOReadCloser(), func() { zstReader.Close() }, nil
	}
	return nil, nil, fmt.Errorf("unsupported archive format %q", format)
}

func extractTar(reader *tar.Reader, target string) error {
	cleanTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	var expanded uint64
	entries := 0
	// Directory modes are applied after every regular file is written so a
	// read-only directory in the archive cannot block its own children.
	deferredDirs := map[string]os.FileMode{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		entries++
		if entries > MaxArchiveFiles {
			return errors.New("archive contains more than 20,000 entries")
		}
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeReg:
		default:
			return fmt.Errorf("archive entry %q has an unsafe file type", header.Name)
		}
		if header.Size > MaxArchiveFile {
			return fmt.Errorf("archive entry %q exceeds 250 MiB", header.Name)
		}
		abs, err := resolveEntry(cleanTarget, header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(abs, 0o750); err != nil {
				return err
			}
			deferredDirs[abs] = 0o750
			continue
		}
		expanded += uint64(header.Size)
		if expanded > MaxArchiveExpanded {
			return errors.New("archive expands beyond 1 GiB")
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return err
		}
		mode := os.FileMode(0o640)
		if header.FileInfo().Mode().Perm()&0o111 != 0 {
			mode = 0o750
		}
		if err := writeTarFile(abs, reader, header.Size, mode); err != nil {
			return err
		}
	}
	for dir, mode := range deferredDirs {
		_ = os.Chmod(dir, mode)
	}
	return nil
}

func writeTarFile(path string, reader io.Reader, size int64, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.CopyN(file, reader, size+1)
	closeErr := file.Close()
	if copyErr != nil && copyErr != io.EOF {
		return copyErr
	}
	if written > size {
		return fmt.Errorf("archive entry %q size mismatch", filepath.Base(path))
	}
	return closeErr
}

func resolveEntry(cleanTarget, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the project directory", name)
	}
	abs, err := filepath.Abs(filepath.Join(cleanTarget, clean))
	if err != nil {
		return "", err
	}
	if abs != cleanTarget && !strings.HasPrefix(abs, cleanTarget+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the project directory", name)
	}
	return abs, nil
}
