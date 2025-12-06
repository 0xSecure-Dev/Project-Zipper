package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xRepo-Source/Project-Zipper/internal/zipper"
)

func main() {
	extractFlag := flag.Bool("x", false, "extract mode: extract zip archive to destination")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options] <source> [destination]\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(flag.CommandLine.Output(), "\nCreate or extract zip archives.")
		fmt.Fprintln(flag.CommandLine.Output(), "CREATE MODE (default):")
		fmt.Fprintln(flag.CommandLine.Output(), "  pz <folder>           Create a zip archive of the folder")
		fmt.Fprintln(flag.CommandLine.Output(), "\nEXTRACT MODE:")
		fmt.Fprintln(flag.CommandLine.Output(), "  pz -x <archive.zip>   Extract archive to current directory")
		fmt.Fprintln(flag.CommandLine.Output(), "  pz -x <archive.zip> <dest>  Extract archive to destination folder")
	}

	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	if *extractFlag {
		doExtract(flag.Args())
	} else {
		doCreate(flag.Args())
	}
}

func doCreate(args []string) {
	target := strings.Join(args, " ")
	absTarget, err := filepath.Abs(target)
	if err != nil {
		exitWithError(err)
	}

	info, err := os.Stat(absTarget)
	if err != nil {
		exitWithError(err)
	}
	if !info.IsDir() {
		exitWithError(errors.New("target must be a directory"))
	}

	parent := filepath.Dir(absTarget)
	base := filepath.Base(absTarget)

	zipPath, err := zipper.NextArchiveName(parent, base)
	if err != nil {
		exitWithError(err)
	}

	printer := newCreateProgressPrinter(absTarget)
	stats, err := zipper.ZipWithProgress(absTarget, zipPath, printer.OnProgress)
	if err != nil {
		exitWithError(err)
	}
	printer.Complete(zipPath, stats)

	fmt.Println(zipPath)
}

func doExtract(args []string) {
	if len(args) < 1 {
		exitWithError(errors.New("extract mode requires a zip file"))
	}

	zipPath := strings.Join(args, " ")
	if len(args) > 1 {
		// If multiple args, first is zip, rest is destination
		zipPath = args[0]
	}

	absZipPath, err := filepath.Abs(zipPath)
	if err != nil {
		exitWithError(err)
	}

	info, err := os.Stat(absZipPath)
	if err != nil {
		exitWithError(err)
	}
	if info.IsDir() {
		exitWithError(errors.New("source must be a zip file, not a directory"))
	}

	// Determine destination
	var destDir string
	if len(args) > 1 {
		destDir = strings.Join(args[1:], " ")
	} else {
		// Extract to current directory
		destDir, err = os.Getwd()
		if err != nil {
			exitWithError(err)
		}
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		exitWithError(err)
	}

	printer := newExtractProgressPrinter(absZipPath, absDestDir)
	stats, err := zipper.ExtractWithProgress(absZipPath, absDestDir, printer.OnProgress)
	if err != nil {
		exitWithError(err)
	}
	printer.Complete(stats)

	fmt.Println(absDestDir)
}

func exitWithError(err error) {
	fmt.Fprintln(os.Stderr, "pz:", err)
	os.Exit(1)
}

// Create mode progress printer
type createProgressPrinter struct {
	source    string
	started   bool
	startTime time.Time
	total     int64
	lastLen   int
}

func newCreateProgressPrinter(source string) *createProgressPrinter {
	return &createProgressPrinter{source: source}
}

func (p *createProgressPrinter) OnProgress(done, total int64) {
	if !p.started {
		p.started = true
		p.startTime = time.Now()
		p.total = total
		fmt.Fprintf(os.Stdout, "Creating archive for %s (%s)...\n", p.source, formatBytes(total))
	}

	line := p.renderLine(done, total)
	p.printLine(line)
}

func (p *createProgressPrinter) renderLine(done, total int64) string {
	const barWidth = 50

	filled := 0
	percent := 100.0
	if total > 0 {
		percent = (float64(done) / float64(total)) * 100
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		filled = int((done * int64(barWidth)) / total)
	} else {
		// Empty directory; treat as complete.
		filled = barWidth
	}
	if filled > barWidth {
		filled = barWidth
	}

	bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
	selapsed := time.Since(p.startTime)
	speed := "0 B/s"
	if selapsed > 0 {
		bytesPerSec := float64(done) / selapsed.Seconds()
		if bytesPerSec < 0 {
			bytesPerSec = 0
		}
		speedValue := int64(bytesPerSec + 0.5)
		speed = fmt.Sprintf("%s/s", formatBytes(speedValue))
	}

	return fmt.Sprintf("[%s] %3.0f%% (%s/%s) %s", bar, percent, formatBytes(done), formatBytes(total), speed)
}

func (p *createProgressPrinter) printLine(line string) {
	if pad := p.lastLen - len(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	fmt.Printf("\r%s", line)
	p.lastLen = len(line)
}

func (p *createProgressPrinter) Complete(zipPath string, stats zipper.ArchiveStats) {
	if !p.started {
		fmt.Println("No files to archive; created empty zip.")
		return
	}
	fmt.Print("\n")
	p.lastLen = 0
	zipInfo, err := os.Stat(zipPath)
	zipSize := int64(0)
	if err == nil {
		zipSize = zipInfo.Size()
	}
	fmt.Fprintf(os.Stdout, "✓ Archive complete: %s -> %s (%s source, %s archive, %d files)\n",
		p.source,
		zipPath,
		formatBytes(stats.TotalBytes),
		formatBytes(zipSize),
		stats.FileCount,
	)
}

// Extract mode progress printer
type extractProgressPrinter struct {
	zipPath   string
	destDir   string
	started   bool
	startTime time.Time
	total     int64
	lastLen   int
}

func newExtractProgressPrinter(zipPath, destDir string) *extractProgressPrinter {
	return &extractProgressPrinter{
		zipPath: zipPath,
		destDir: destDir,
	}
}

func (p *extractProgressPrinter) OnProgress(done, total int64) {
	if !p.started {
		p.started = true
		p.startTime = time.Now()
		p.total = total
		fmt.Fprintf(os.Stdout, "Extracting %s (%s)...\n", filepath.Base(p.zipPath), formatBytes(total))
	}

	line := p.renderLine(done, total)
	p.printLine(line)
}

func (p *extractProgressPrinter) renderLine(done, total int64) string {
	const barWidth = 50

	filled := 0
	percent := 100.0
	if total > 0 {
		percent = (float64(done) / float64(total)) * 100
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		filled = int((done * int64(barWidth)) / total)
	} else {
		filled = barWidth
	}
	if filled > barWidth {
		filled = barWidth
	}

	bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
	selapsed := time.Since(p.startTime)
	speed := "0 B/s"
	if selapsed > 0 {
		bytesPerSec := float64(done) / selapsed.Seconds()
		if bytesPerSec < 0 {
			bytesPerSec = 0
		}
		speedValue := int64(bytesPerSec + 0.5)
		speed = fmt.Sprintf("%s/s", formatBytes(speedValue))
	}

	return fmt.Sprintf("[%s] %3.0f%% (%s/%s) %s", bar, percent, formatBytes(done), formatBytes(total), speed)
}

func (p *extractProgressPrinter) printLine(line string) {
	if pad := p.lastLen - len(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	fmt.Printf("\r%s", line)
	p.lastLen = len(line)
}

func (p *extractProgressPrinter) Complete(stats zipper.ExtractStats) {
	if !p.started {
		fmt.Println("No files extracted.")
		return
	}
	fmt.Print("\n")
	p.lastLen = 0
	fmt.Fprintf(os.Stdout, "✓ Extraction complete: %s -> %s (%s extracted, %d files)\n",
		filepath.Base(p.zipPath),
		p.destDir,
		formatBytes(stats.TotalBytes),
		stats.FileCount,
	)
}

type progressPrinter = createProgressPrinter

func newProgressPrinter(source string) *progressPrinter {
	return newCreateProgressPrinter(source)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	suffixes := []string{"KB", "MB", "GB", "TB", "PB"}
	div := float64(unit)
	exp := 0
	for n/int64(div) >= unit && exp < len(suffixes)-1 {
		div *= unit
		exp++
	}
	value := float64(n) / div
	return fmt.Sprintf("%.1f %s", value, suffixes[exp])
}
