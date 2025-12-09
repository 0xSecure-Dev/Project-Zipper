package zipper

import (
	"archive/tar"
	"archive/zip"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ProgressFunc reports the number of source bytes processed out of the total.
type ProgressFunc func(done, total int64)

// ArchiveStats describes the payload processed while creating an archive.
type ArchiveStats struct {
	TotalBytes int64
	FileCount  int
	Checksum   string // SHA-256 checksum of the archive
}

// getWorkerCount returns the number of workers to use (50% of CPU cores, minimum 1)
func getWorkerCount() int {
	numCPU := runtime.NumCPU()
	workers := numCPU / 2
	if workers < 1 {
		workers = 1
	}
	return workers
}

// fileJob represents a file to be compressed
type fileJob struct {
	path  string
	rel   string
	info  fs.FileInfo
	isDir bool
}

// getCompressionMethod returns the optimal compression method for a file
// Returns zip.Store for already-compressed files, zip.Deflate for everything else
func getCompressionMethod(filename string) uint16 {
	ext := strings.ToLower(filepath.Ext(filename))
	// Already compressed formats - store without recompression
	noCompress := map[string]bool{
		".zip": true, ".gz": true, ".7z": true, ".rar": true,
		".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
		".mp3": true, ".mp4": true, ".avi": true, ".mkv": true, ".mov": true,
		".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	}
	if noCompress[ext] {
		return zip.Store
	}
	return zip.Deflate
}

// getOptimalCompressionLevel returns compression level based on total archive size
// Larger archives use faster compression, smaller archives get better compression
func getOptimalCompressionLevel(totalSize int64) int {
	const MB = 1024 * 1024
	switch {
	case totalSize < 10*MB:
		return flate.BestCompression // Small files: max compression
	case totalSize < 100*MB:
		return flate.DefaultCompression // Medium: balanced
	case totalSize < 500*MB:
		return 4 // Large: favor speed
	default:
		return flate.BestSpeed // Very large: maximum speed
	}
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

	writer := zip.NewWriter(zipFile)
	// Register custom compressor with optimal level based on total size
	compressionLevel := getOptimalCompressionLevel(stats.TotalBytes)
	writer.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, compressionLevel)
	})

	done := int64(0)
	var doneMutex sync.Mutex
	callProgress := func() {
		if progress != nil {
			doneMutex.Lock()
			progress(done, stats.TotalBytes)
			doneMutex.Unlock()
		}
	}
	callProgress()

	// Collect all files first
	var files []fileJob
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

		files = append(files, fileJob{
			path:  path,
			rel:   rel,
			info:  info,
			isDir: d.IsDir(),
		})
		return nil
	})
	if err != nil {
		return stats, err
	}

	// Process files with worker pool for reading
	workerCount := getWorkerCount()
	type fileData struct {
		job  fileJob
		data []byte
		err  error
	}

	dataChan := make(chan fileData, workerCount)
	var wg sync.WaitGroup

	// Start workers to read files in parallel
	jobChan := make(chan fileJob, len(files))
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				if job.isDir {
					dataChan <- fileData{job: job}
					continue
				}

				data, err := os.ReadFile(job.path)
				dataChan <- fileData{
					job:  job,
					data: data,
					err:  err,
				}
			}
		}()
	}

	// Send jobs to workers
	go func() {
		for _, file := range files {
			jobChan <- file
		}
		close(jobChan)
	}()

	// Close data channel when all workers finish
	go func() {
		wg.Wait()
		close(dataChan)
	}()

	// Write to zip sequentially (required by zip format)
	processedCount := 0
	for fd := range dataChan {
		if fd.err != nil {
			return stats, fd.err
		}

		header, err := zip.FileInfoHeader(fd.job.info)
		if err != nil {
			return stats, err
		}

		header.Name = filepath.ToSlash(fd.job.rel)
		if fd.job.isDir {
			header.Name += "/"
		} else {
			header.Method = getCompressionMethod(fd.job.path)
		}

		writerEntry, err := writer.CreateHeader(header)
		if err != nil {
			return stats, err
		}

		if !fd.job.isDir {
			_, err = writerEntry.Write(fd.data)
			if err != nil {
				return stats, err
			}

			doneMutex.Lock()
			done += int64(len(fd.data))
			doneMutex.Unlock()

			if progress != nil {
				callProgress()
			}
		}

		processedCount++
	}

	callProgress()

	// Close writer and file explicitly before calculating checksum
	if err := writer.Close(); err != nil {
		return stats, err
	}
	if err := zipFile.Close(); err != nil {
		return stats, err
	}

	// Calculate checksum of the created archive
	stats.Checksum, err = calculateFileChecksum(zipPath)
	if err != nil {
		return stats, fmt.Errorf("checksum calculation failed: %w", err)
	}

	// Store checksum in zip comment
	if err := addChecksumToZip(zipPath, stats.Checksum); err != nil {
		return stats, fmt.Errorf("failed to add checksum: %w", err)
	}

	return stats, nil
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
	var doneMutex sync.Mutex
	callProgress := func() {
		if progress != nil {
			doneMutex.Lock()
			progress(done, totalBytes)
			doneMutex.Unlock()
		}
	}
	callProgress()

	// Create directories first
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			destPath := filepath.Join(destDir, filepath.FromSlash(f.Name))
			if !filepath.IsLocal(f.Name) {
				return stats, fmt.Errorf("invalid file path: %s", f.Name)
			}
			if err := os.MkdirAll(destPath, f.Mode()); err != nil {
				return stats, err
			}
		}
	}

	// Extract files in parallel
	workerCount := getWorkerCount()
	type extractJob struct {
		file     *zip.File
		destPath string
	}

	jobChan := make(chan extractJob, len(reader.File))
	errChan := make(chan error, 1)
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				rc, err := job.file.Open()
				if err != nil {
					select {
					case errChan <- err:
					default:
					}
					return
				}

				outFile, err := os.OpenFile(job.destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, job.file.Mode())
				if err != nil {
					rc.Close()
					select {
					case errChan <- err:
					default:
					}
					return
				}

				written, err := io.Copy(outFile, rc)
				rc.Close()
				outFile.Close()

				if err != nil {
					select {
					case errChan <- err:
					default:
					}
					return
				}

				doneMutex.Lock()
				done += written
				doneMutex.Unlock()

				if progress != nil {
					callProgress()
				}
			}
		}()
	}

	// Send jobs
	go func() {
		for _, f := range reader.File {
			if f.FileInfo().IsDir() {
				continue
			}

			destPath := filepath.Join(destDir, filepath.FromSlash(f.Name))

			// Security check: prevent path traversal
			if !filepath.IsLocal(f.Name) {
				select {
				case errChan <- fmt.Errorf("invalid file path: %s", f.Name):
				default:
				}
				break
			}

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				select {
				case errChan <- err:
				default:
				}
				break
			}

			jobChan <- extractJob{file: f, destPath: destPath}
		}
		close(jobChan)
	}()

	// Wait for completion
	wg.Wait()
	close(errChan)

	// Check for errors
	if err := <-errChan; err != nil {
		return stats, err
	}

	callProgress()
	return stats, nil
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

// Gzip creates a tar.gz archive of the source directory
func Gzip(srcDir, gzipPath string) error {
	_, err := GzipWithProgress(srcDir, gzipPath, nil)
	return err
}

// GzipWithProgress creates a tar.gz archive and reports progress via callback
func GzipWithProgress(srcDir, gzipPath string, progress ProgressFunc) (stats ArchiveStats, err error) {
	stats, err = scanDirectory(srcDir)
	if err != nil {
		return stats, err
	}

	gzipFile, err := os.Create(gzipPath)
	if err != nil {
		return stats, err
	}

	// Use optimal compression level based on total size
	compressionLevel := getOptimalCompressionLevel(stats.TotalBytes)
	gzWriter, err := gzip.NewWriterLevel(gzipFile, compressionLevel)
	if err != nil {
		return stats, err
	}

	tarWriter := tar.NewWriter(gzWriter)

	done := int64(0)
	var doneMutex sync.Mutex
	callProgress := func() {
		if progress != nil {
			doneMutex.Lock()
			progress(done, stats.TotalBytes)
			doneMutex.Unlock()
		}
	}
	callProgress()

	// Collect all files first
	var files []fileJob
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

		files = append(files, fileJob{
			path:  path,
			rel:   rel,
			info:  info,
			isDir: d.IsDir(),
		})
		return nil
	})
	if err != nil {
		return stats, err
	}

	// Process files with worker pool for reading
	workerCount := getWorkerCount()
	type fileData struct {
		job  fileJob
		data []byte
		err  error
	}

	dataChan := make(chan fileData, workerCount)
	var wg sync.WaitGroup

	// Start workers to read files in parallel
	jobChan := make(chan fileJob, len(files))
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				if job.isDir {
					dataChan <- fileData{job: job}
					continue
				}

				data, err := os.ReadFile(job.path)
				dataChan <- fileData{
					job:  job,
					data: data,
					err:  err,
				}
			}
		}()
	}

	// Send jobs to workers
	go func() {
		for _, file := range files {
			jobChan <- file
		}
		close(jobChan)
	}()

	// Close data channel when all workers finish
	go func() {
		wg.Wait()
		close(dataChan)
	}()

	// Write to tar sequentially (required by tar format)
	for fd := range dataChan {
		if fd.err != nil {
			return stats, fd.err
		}

		header, err := tar.FileInfoHeader(fd.job.info, "")
		if err != nil {
			return stats, err
		}

		header.Name = filepath.ToSlash(fd.job.rel)

		if err := tarWriter.WriteHeader(header); err != nil {
			return stats, err
		}

		if !fd.job.isDir {
			_, err = tarWriter.Write(fd.data)
			if err != nil {
				return stats, err
			}

			doneMutex.Lock()
			done += int64(len(fd.data))
			doneMutex.Unlock()

			if progress != nil {
				callProgress()
			}
		}
	}

	callProgress()

	// Close writers explicitly before calculating checksum
	if err := tarWriter.Close(); err != nil {
		return stats, err
	}
	if err := gzWriter.Close(); err != nil {
		return stats, err
	}
	if err := gzipFile.Close(); err != nil {
		return stats, err
	}

	// Calculate checksum of the created archive
	stats.Checksum, err = calculateFileChecksum(gzipPath)
	if err != nil {
		return stats, fmt.Errorf("checksum calculation failed: %w", err)
	}

	// Store checksum in a separate .sha256 file
	if err := writeChecksumFile(gzipPath, stats.Checksum); err != nil {
		return stats, fmt.Errorf("failed to write checksum file: %w", err)
	}

	return stats, nil
}

// ExtractGzip extracts a tar.gz archive to the destination directory
func ExtractGzip(gzipPath, destDir string) error {
	_, err := ExtractGzipWithProgress(gzipPath, destDir, nil)
	return err
}

// ExtractGzipWithProgress extracts a tar.gz archive and reports progress via callback
func ExtractGzipWithProgress(gzipPath, destDir string, progress ProgressFunc) (stats ExtractStats, err error) {
	gzipFile, err := os.Open(gzipPath)
	if err != nil {
		return stats, err
	}
	defer gzipFile.Close()

	// First pass: calculate total size
	gzReader, err := gzip.NewReader(gzipFile)
	if err != nil {
		return stats, err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)
	totalBytes := int64(0)
	fileCount := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stats, err
		}
		if header.Typeflag == tar.TypeReg {
			totalBytes += header.Size
			fileCount++
		}
	}

	stats.TotalBytes = totalBytes
	stats.FileCount = fileCount

	// Reopen for actual extraction
	gzipFile.Seek(0, 0)
	gzReader2, err := gzip.NewReader(gzipFile)
	if err != nil {
		return stats, err
	}
	defer gzReader2.Close()

	tarReader2 := tar.NewReader(gzReader2)

	done := int64(0)
	callProgress := func() {
		if progress != nil {
			progress(done, totalBytes)
		}
	}
	callProgress()

	for {
		header, err := tarReader2.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stats, err
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(header.Name))

		// Security check: prevent path traversal
		if !filepath.IsLocal(header.Name) {
			return stats, fmt.Errorf("invalid file path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, os.FileMode(header.Mode)); err != nil {
				return stats, err
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return stats, err
			}

			outFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return stats, err
			}

			pr := &progressReader{
				r:        tarReader2,
				done:     &done,
				total:    totalBytes,
				progress: progress,
			}

			if _, err = io.Copy(outFile, pr); err != nil {
				outFile.Close()
				return stats, err
			}
			if err := outFile.Close(); err != nil {
				return stats, err
			}
		}
	}

	callProgress()
	return stats, nil
}

// calculateFileChecksum computes SHA-256 checksum of a file
func calculateFileChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// addChecksumToZip adds the checksum to the zip file comment
func addChecksumToZip(zipPath, checksum string) error {
	// Read the zip file
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}

	// Create a temporary file
	tempPath := zipPath + ".tmp"
	tempFile, err := os.Create(tempPath)
	if err != nil {
		r.Close()
		return err
	}

	// Create new zip writer
	w := zip.NewWriter(tempFile)
	w.SetComment(fmt.Sprintf("SHA256: %s", checksum))

	// Copy all files from original zip
	for _, f := range r.File {
		if err := copyZipFile(w, f); err != nil {
			w.Close()
			tempFile.Close()
			r.Close()
			os.Remove(tempPath)
			return err
		}
	}

	r.Close()
	if err := w.Close(); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return err
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}

	// Replace original with temp
	if err := os.Remove(zipPath); err != nil {
		return err
	}
	return os.Rename(tempPath, zipPath)
}

// copyZipFile copies a file from one zip to another
func copyZipFile(w *zip.Writer, f *zip.File) error {
	fw, err := w.CreateHeader(&f.FileHeader)
	if err != nil {
		return err
	}

	fr, err := f.Open()
	if err != nil {
		return err
	}
	defer fr.Close()

	_, err = io.Copy(fw, fr)
	return err
}

// writeChecksumFile writes checksum to a .sha256 file
func writeChecksumFile(archivePath, checksum string) error {
	checksumPath := archivePath + ".sha256"
	content := fmt.Sprintf("%s *%s\n", checksum, filepath.Base(archivePath))
	return os.WriteFile(checksumPath, []byte(content), 0644)
}

// VerifyChecksum verifies the checksum of an archive
func VerifyChecksum(archivePath string) (bool, string, error) {
	ext := strings.ToLower(filepath.Ext(archivePath))

	if ext == ".zip" {
		// Read checksum from zip comment
		r, err := zip.OpenReader(archivePath)
		if err != nil {
			return false, "", err
		}
		defer r.Close()

		comment := r.Comment
		if !strings.HasPrefix(comment, "SHA256: ") {
			return false, "", fmt.Errorf("no checksum found in archive")
		}

		storedChecksum := strings.TrimPrefix(comment, "SHA256: ")
		actualChecksum, err := calculateFileChecksum(archivePath)
		if err != nil {
			return false, "", err
		}

		return storedChecksum == actualChecksum, storedChecksum, nil
	} else if ext == ".gz" || strings.HasSuffix(archivePath, ".tar.gz") {
		// Read from .sha256 file
		checksumPath := archivePath + ".sha256"
		data, err := os.ReadFile(checksumPath)
		if err != nil {
			return false, "", fmt.Errorf("checksum file not found: %w", err)
		}

		parts := strings.Fields(string(data))
		if len(parts) < 1 {
			return false, "", fmt.Errorf("invalid checksum file format")
		}

		storedChecksum := parts[0]
		actualChecksum, err := calculateFileChecksum(archivePath)
		if err != nil {
			return false, "", err
		}

		return storedChecksum == actualChecksum, storedChecksum, nil
	}

	return false, "", fmt.Errorf("unsupported archive format")
}
