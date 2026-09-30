// Package portsmith is the native Go port of the Portsmith migration
// executor. files.go ports src/files.ts: strict relative paths, symbolic-link
// rejection, SHA-256 hashing, atomic JSON writes, exclusive locks and
// deterministic file snapshots/copies.
package portsmith

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// File is one in-memory file record (upstream `{name, data, sha256}`).
type File struct {
	Name   string
	Data   []byte
	SHA256 string
}

// Hash returns the lowercase SHA-256 hex digest of data, matching
// `createHash("sha256").update(data).digest("hex")`.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RelativeName validates and returns a strict, slash-separated relative path.
// It mirrors the upstream `relativeName` guard: no empty input, no absolute
// paths, no backslashes, no NUL bytes and no empty/. /.. path segments.
func RelativeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("Invalid relative path: %s", name)
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("Invalid relative path: %s", name)
	}
	if strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("Invalid relative path: %s", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("Invalid relative path: %s", name)
		}
	}
	return name, nil
}

// CheckedFile resolves a relative path below root and rejects symbolic links
// anywhere in the path. The final component must be a regular file.
func CheckedFile(root, name string) (string, error) {
	if _, err := RelativeName(name); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Symbolic links are not allowed")
	}
	current := root
	for _, part := range strings.Split(name, "/") {
		current = filepath.Join(current, filepath.FromSlash(part))
		link, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if link.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("Symbolic links are not allowed")
		}
	}
	link, err := os.Lstat(current)
	if err != nil {
		return "", err
	}
	if !link.Mode().IsRegular() {
		return "", fmt.Errorf("Expected a regular file: %s", name)
	}
	return current, nil
}

// readJSON reads and decodes a checked JSON file below root.
func readJSON[T any](root, name string) (T, error) {
	var zero T
	file, err := CheckedFile(root, name)
	if err != nil {
		return zero, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return zero, err
	}
	if err := json.Unmarshal(data, &zero); err != nil {
		return zero, err
	}
	return zero, nil
}

// AtomicJSON writes value as pretty JSON through a temporary file and renames
// it into place. The output matches `JSON.stringify(value, null, 2) + "\n"`.
func AtomicJSON(file string, value any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.%s.tmp", file, randomToken())
	handle, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := handle.Write(buf.Bytes()); err != nil {
		handle.Close()
		os.Remove(temp)
		return err
	}
	if err := handle.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, file); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

// WithLock runs fn while holding an exclusive `.lock` file at the resolved
// root. The lock is always released, including when fn fails.
func WithLock(ctx context.Context, rootInput string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(rootInput)
	if err != nil {
		return err
	}
	lock := filepath.Join(root, ".lock")
	handle, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("Task is already running. After an interrupted run, confirm its process has exited before removing .lock.")
		}
		return err
	}
	if _, err := fmt.Fprintf(handle, "%d\n", os.Getpid()); err != nil {
		handle.Close()
		os.Remove(lock)
		return err
	}
	if err := handle.Close(); err != nil {
		os.Remove(lock)
		return err
	}
	defer os.Remove(lock)
	return fn()
}

// snapshotFiles walks root and returns every regular file in deterministic
// locale order. Zero maxFiles/maxBytes/maxFileBytes values mean unlimited.
func snapshotFiles(root string, maxFiles, maxBytes int, maxFileBytes func(string) int) ([]File, error) {
	if maxFileBytes == nil {
		maxFileBytes = func(string) int { return 0 }
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Symbolic links are not allowed")
	}
	var files []File
	total := 0
	var visit func(dir string) error
	visit = func(dir string) error {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			return err
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return nodeLocaleCompare(entries[i].Name(), entries[j].Name()) < 0
		})
		for _, entry := range entries {
			name := path.Join(dir, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("Symbolic links are not allowed：%s", name)
			}
			if entry.IsDir() {
				if err := visit(name); err != nil {
					return err
				}
				continue
			}
			file, err := CheckedFile(root, name)
			if err != nil {
				return err
			}
			stat, err := os.Lstat(file)
			if err != nil {
				return err
			}
			limit := maxBytes
			if perFile := maxFileBytes(name); perFile > 0 && (limit == 0 || perFile < limit) {
				limit = perFile
			}
			if limit > 0 && stat.Size() > int64(limit) {
				return fmt.Errorf("File exceeds the configured size limit: %s", name)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			files = append(files, File{Name: name, Data: data, SHA256: Hash(data)})
			total += len(data)
			if (maxFiles > 0 && len(files) > maxFiles) || (maxBytes > 0 && total > maxBytes) {
				return errors.New("Task exceeds the configured size limit; split it into smaller modules")
			}
		}
		return nil
	}
	if err := visit(""); err != nil {
		return nil, err
	}
	return files, nil
}

// copyFiles writes each record below root, refusing to overwrite an existing
// file (upstream `writeFile(..., { flag: "wx" })`).
func copyFiles(root string, files []File) error {
	for _, f := range files {
		if _, err := RelativeName(f.Name); err != nil {
			return err
		}
		dest := filepath.Join(root, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		handle, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := handle.Write(f.Data); err != nil {
			handle.Close()
			return err
		}
		if err := handle.Close(); err != nil {
			return err
		}
	}
	return nil
}

// randomToken returns a random 128-bit hex token used for atomic temp names.
func randomToken() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(raw[:])
}

// nodeLocaleCompare approximates JavaScript's default `String.localeCompare`
// for ASCII file names. The order string is the ICU root/general collation
// order observed for printable ASCII, which keeps deterministic snapshot
// ordering close to the Node source.
var collationWeights = func() map[rune]int {
	order := []rune(" _- ,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$0123456789aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStTuUvVwWxXyYzZ")
	weights := make(map[rune]int, len(order)+1)
	for i, r := range order {
		if _, exists := weights[r]; !exists {
			weights[r] = i
		}
	}
	return weights
}()

func collationWeight(r rune) int {
	if weight, ok := collationWeights[r]; ok {
		return weight
	}
	return 1000 + int(r)
}

func nodeLocaleCompare(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	for i := 0; i < len(ar) && i < len(br); i++ {
		wa, wb := collationWeight(ar[i]), collationWeight(br[i])
		if wa != wb {
			return wa - wb
		}
	}
	return len(ar) - len(br)
}
