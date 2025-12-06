# Project Zipper

`pz` is a lightweight Go CLI that creates and extracts zip archives using only the Go standard library. It generates unique archive names by appending version suffixes when a zip with the base name already exists.

## Prerequisites

- Go 1.21 or later

## Install

```powershell
# From the project root
go install ./cmd/pzip
```

This installs the `pzip` binary into your `$GOBIN` (or `$GOPATH/bin`).

## Usage

### Create Archive

```powershell
pz <path-to-folder>
```

- Archives the specified folder into `<folder>.zip` alongside the source folder.
- If `<folder>.zip` already exists, a versioned archive such as `<folder>-v1.zip`, `<folder>-v2.zip`, etc. is created instead.
- Paths containing spaces are supported without quoting (e.g. `pz C:\Active Projects`).

### Extract Archive

```powershell
# Extract to current directory
pz -x <archive.zip>

# Extract to specific destination
pz -x <archive.zip> <destination-folder>
```

- Extracts the contents of a zip archive
- Creates destination directory if it doesn't exist
- Shows progress bar with extraction speed
- Includes path traversal protection for security

### Progress Output

**Creating Archive:**
```text
Creating archive for H:\Example\Project (6.1 MB)...
[##############################--------------------] 62% (3.8 MB/6.1 MB) 4.2 MB/s
Archive complete: H:\Example\Project -> H:\Example\Project.zip (6.1 MB source, 2.9 MB archive, 12 files)
```

**Extracting Archive:**
```text
Extracting Project.zip (2.9 MB)...
[##################################################] 100% (6.1 MB/6.1 MB) 8.3 MB/s
Extraction complete: Project.zip -> H:\Example\Extracted (6.1 MB extracted, 12 files)
```

## Windows Env

To add the tool to the system `env` you can copy the pz.exe from `bin\pz.exe` to `C:\Program files\pz\pz.exe`.
Then `start` type `env` click `Enviroment Variables` select `Path > Edit > New`.
Then paste `C:\Program files\pz\`.

Open `cmd / powershell` type `pz` and you should get `Usage: pz <folder>`.

## Development

```powershell
# Format code
gofmt -w .

# Build and verify
go build ./...

# Run tests
go test ./...
```

`pzip` Can be renamed to `pz` when you run `go build -o pz.exe`

## Continuous Integration

This repository includes a GitHub Actions workflow (`.github/workflows/ci.yml`) that checks formatting and runs the test suite on each push and pull request.

## License

MIT License. See [`LICENSE`](LICENSE) for details.
