//go:build ignore

// Build release archives with Go only. No signing, tag creation, or publishing
// occurs here. Run from the repository root with an empty output directory.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const allTargets = "darwin/arm64,darwin/amd64,linux/arm64,linux/amd64,windows/amd64"

type archiveFile struct {
	Name string
	Data []byte
	Mode int64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return output, nil
}

func run() error {
	version := flag.String("version", "dev", "Archive version (dev or an existing vMAJOR.MINOR.PATCH tag)")
	output := flag.String("out", "dist", "Empty output directory")
	targets := flag.String("targets", allTargets, "Comma-separated release targets; defaults to all supported targets")
	flag.Parse()
	if flag.NArg() != 0 || !regexp.MustCompile(`^(dev|v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9][A-Za-z0-9.-]*)?)$`).MatchString(*version) {
		return fmt.Errorf("invalid archive version or unexpected arguments")
	}
	allowed := make(map[string]bool)
	for _, target := range strings.Split(allTargets, ",") {
		allowed[target] = true
	}
	selected := strings.Split(*targets, ",")
	seen := make(map[string]bool)
	for _, target := range selected {
		if !allowed[target] || seen[target] {
			return fmt.Errorf("unsupported or repeated target: %s", target)
		}
		seen[target] = true
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(*output)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("output directory must be empty: %s", *output)
	}
	if _, err := command("go", "mod", "download", "all"); err != nil {
		return err
	}
	files, err := licenseFiles()
	if err != nil {
		return err
	}
	revision, err := command("git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	files = append(files, archiveFile{"BUILD.txt", []byte("Version: " + *version + "\nCommit: " + strings.TrimSpace(string(revision)) + "\nCGO_ENABLED: 0\n"), 0o644})
	buildDir, err := os.MkdirTemp("", "portsmith-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildDir)
	var checksums strings.Builder
	for _, target := range selected {
		parts := strings.Split(target, "/")
		binaryName := "portsmith"
		extension := ".tar.gz"
		if parts[0] == "windows" {
			binaryName += ".exe"
			extension = ".zip"
		}
		binaryPath := filepath.Join(buildDir, binaryName)
		cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-ldflags=-s -w", "-o", binaryPath, "./cmd/portsmith")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOOS="+parts[0], "GOARCH="+parts[1])
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w\n%s", target, err, output)
		}
		binary, err := os.ReadFile(binaryPath)
		if err != nil {
			return err
		}
		name := "portsmith-go_" + *version + "_" + parts[0] + "_" + parts[1] + extension
		payload := append(append([]archiveFile(nil), files...), archiveFile{binaryName, binary, 0o755})
		if err := writeArchive(filepath.Join(*output, name), payload, extension == ".zip"); err != nil {
			return err
		}
		archive, err := os.ReadFile(filepath.Join(*output, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&checksums, "%x  %s\n", sha256.Sum256(archive), name)
		fmt.Println(name)
	}
	return os.WriteFile(filepath.Join(*output, "checksums.txt"), []byte(checksums.String()), 0o644)
}

func licenseFiles() ([]archiveFile, error) {
	var files []archiveFile
	add := func(name, source string) error {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		files = append(files, archiveFile{filepath.ToSlash(name), data, 0o644})
		return nil
	}
	for name, source := range map[string]string{
		"LICENSE":                         "LICENSE",
		"THIRD_PARTY_NOTICES.md":          "docs/THIRD_PARTY.md",
		"licenses/typescript-LICENSE.txt": "internal/portsmith/typescript-LICENSE.txt",
		"licenses/typescript-NOTICE.txt":  "internal/portsmith/typescript-NOTICE.txt",
		"licenses/goja-LICENSE.txt":       "internal/portsmith/goja-LICENSE.txt",
		"licenses/pith-LICENSE.txt":       "internal/portsmith/pith-LICENSE.txt",
	} {
		if err := add(name, source); err != nil {
			return nil, err
		}
	}
	goRoot, err := command("go", "env", "GOROOT")
	if err != nil {
		return nil, err
	}
	goLicense, err := goLicensePath(strings.TrimSpace(string(goRoot)))
	if err != nil {
		return nil, err
	}
	if err := add("licenses/Go-LICENSE.txt", goLicense); err != nil {
		return nil, err
	}
	modules, err := command("go", "list", "-m", "-json", "-mod=readonly", "all")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(modules))
	for {
		var module struct {
			Path, Version, Dir string
			Main               bool
		}
		if err := decoder.Decode(&module); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if module.Main {
			continue
		}
		entries, err := os.ReadDir(module.Dir)
		if err != nil {
			return nil, fmt.Errorf("read licenses for %s: %w", module.Path, err)
		}
		count := 0
		for _, entry := range entries {
			name := strings.ToLower(entry.Name())
			if entry.IsDir() || !(strings.Contains(name, "license") || strings.Contains(name, "copying") || strings.Contains(name, "notice") || strings.Contains(name, "copyright")) {
				continue
			}
			if err := add(filepath.Join("licenses", "modules", module.Path+"@"+module.Version, entry.Name()), filepath.Join(module.Dir, entry.Name())); err != nil {
				return nil, err
			}
			count++
		}
		if count == 0 {
			return nil, fmt.Errorf("no license or notice found for %s@%s", module.Path, module.Version)
		}
	}
	return files, nil
}

// Homebrew installs the Go license beside libexec rather than inside GOROOT.
// Use only the installed toolchain's license, and fail if it is unavailable.
func goLicensePath(goRoot string) (string, error) {
	paths := []string{filepath.Join(goRoot, "LICENSE")}
	if filepath.Base(filepath.Clean(goRoot)) == "libexec" {
		paths = append(paths, filepath.Join(filepath.Dir(goRoot), "LICENSE"))
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("Go license is not a regular file: %s", path)
		}
		return path, nil
	}
	return "", fmt.Errorf("Go toolchain license not found in %s", strings.Join(paths, ", "))
}

func writeArchive(path string, files []archiveFile, windows bool) (err error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	if windows {
		writer := zip.NewWriter(out)
		for _, file := range files {
			header := &zip.FileHeader{Name: file.Name, Method: zip.Deflate}
			header.SetMode(os.FileMode(file.Mode))
			header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
			entry, err := writer.CreateHeader(header)
			if err != nil {
				return err
			}
			if _, err := entry.Write(file.Data); err != nil {
				return err
			}
		}
		return writer.Close()
	}
	compressed := gzip.NewWriter(out)
	writer := tar.NewWriter(compressed)
	for _, file := range files {
		if err := writer.WriteHeader(&tar.Header{Name: file.Name, Mode: file.Mode, Size: int64(len(file.Data)), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		if _, err := writer.Write(file.Data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return compressed.Close()
}
