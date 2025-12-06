package zipper

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ProgressFunc reports the number of source bytes processed out of the total.
type ProgressFunc func(done, total int64)

// ArchiveStats describes the payload processed while creating an archive.
type ArchiveStats struct {
	TotalBytes int64
	FileCount  int
}

// Zip archives the contents of srcDir into zipPath using only the Go standard library.
func Zip(srcDir, zipPath string) error {
	_, err := ZipWithProgress(srcDir, zipPath, nil)
	return err
}

// ZipWithProgress is identical to Zip but reports progress via the callback.
func ZipWithProgress(srcDir, zipPath string, progress ProgressFunc) (stats ArchiveStats, err error) {
	stats, err = scanDirectory(srcDir)
	if err != nil {
		return stats, err
	}

	zipFile, err := os.Create(zipPath)
	if err != nil {
		return stats, err
	}
	defer func() {
		if cerr := zipFile.Close(); err == nil {
			err = cerr
		}
	}()

	writer := zip.NewWriter(zipFile)
	defer func() {
		if cerr := writer.Close(); err == nil {
			err = cerr
		}
	}()

	done := int64(0)
	callProgress := func() {
		if progress != nil {
			progress(done, stats.TotalBytes)
		}
	}
	callProgress()

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		header.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writerEntry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}

		pw := progressWriter{
			w:        writerEntry,
			done:     &done,
			total:    stats.TotalBytes,
			progress: progress,
		}

		if _, err = io.Copy(&pw, file); err != nil {
			file.Close()
			return err
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}

		return nil
	})
	if err != nil {
		return stats, err
	}

	callProgress()
	return stats, err
}

type progressWriter struct {
	w        io.Writer
	done     *int64
	total    int64
	progress ProgressFunc
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if pw.progress != nil && n > 0 {
		*pw.done += int64(n)
		pw.progress(*pw.done, pw.total)
	}
	return n, err
}

func scanDirectory(root string) (ArchiveStats, error) {
	stats := ArchiveStats{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		stats.TotalBytes += info.Size()
		stats.FileCount++
		return nil
	})
	return stats, err
}

// ExtractStats describes the data extracted from an archive.
type ExtractStats struct {
	TotalBytes int64
	FileCount  int
}

// Extract extracts a zip archive to the destination directory.
func Extract(zipPath, destDir string) error {
	_, err := ExtractWithProgress(zipPath, destDir, nil)
	return err
}

// ExtractWithProgress extracts a zip archive and reports progress via callback.
func ExtractWithProgress(zipPath, destDir string, progress ProgressFunc) (stats ExtractStats, err error) {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return stats, err
	}
	defer reader.Close()

	// Calculate total size
	totalBytes := int64(0)
	fileCount := 0
	for _, f := range reader.File {
		if !f.FileInfo().IsDir() {
			totalBytes += int64(f.UncompressedSize64)
			fileCount++
		}
	}

	stats.TotalBytes = totalBytes
	stats.FileCount = fileCount

	done := int64(0)
	callProgress := func() {
		if progress != nil {
			progress(done, totalBytes)
		}
	}
	callProgress()

	for _, f := range reader.File {
		destPath := filepath.Join(destDir, filepath.FromSlash(f.Name))

		// Security check: prevent path traversal
		if !filepath.IsLocal(f.Name) {
			return stats, fmt.Errorf("invalid file path: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, f.Mode()); err != nil {
				return stats, err
			}
			continue
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return stats, err
		}

		// Extract file
		if err := extractFile(f, destPath, &done, totalBytes, progress); err != nil {
			return stats, err
		}
	}

	callProgress()
	return stats, nil
}

func extractFile(f *zip.File, destPath string, done *int64, total int64, progress ProgressFunc) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	outFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer outFile.Close()

	pr := &progressReader{
		r:        rc,
		done:     done,
		total:    total,
		progress: progress,
	}

	_, err = io.Copy(outFile, pr)
	return err
}

type progressReader struct {
	r        io.Reader
	done     *int64
	total    int64
	progress ProgressFunc
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if pr.progress != nil && n > 0 {
		*pr.done += int64(n)
		pr.progress(*pr.done, pr.total)
	}
	return n, err
}
