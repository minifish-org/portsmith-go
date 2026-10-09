//go:build ignore

// Run with: go test ./scripts/release.go ./scripts/release_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoLicensePath(t *testing.T) {
	for _, tc := range []struct {
		name, rootName                               string
		rootLicense, parentLicense, licenseDirectory bool
		wantParent, wantError                        bool
	}{
		{name: "standard toolchain", rootName: "go", rootLicense: true},
		{name: "Homebrew toolchain", rootName: "libexec", parentLicense: true, wantParent: true},
		{name: "root takes precedence", rootName: "libexec", rootLicense: true, parentLicense: true},
		{name: "missing Homebrew license", rootName: "libexec", wantError: true},
		{name: "unrelated parent is rejected", rootName: "go", parentLicense: true, wantError: true},
		{name: "directory is rejected", rootName: "libexec", licenseDirectory: true, parentLicense: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, tc.rootName)
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			for path, enabled := range map[string]bool{
				filepath.Join(root, "LICENSE"):   tc.rootLicense,
				filepath.Join(parent, "LICENSE"): tc.parentLicense,
			} {
				if enabled {
					if err := os.WriteFile(path, []byte("toolchain license"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.licenseDirectory {
				if err := os.Mkdir(filepath.Join(root, "LICENSE"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			got, err := goLicensePath(root)
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted invalid license path %q", got)
				}
				return
			}
			want := filepath.Join(root, "LICENSE")
			if tc.wantParent {
				want = filepath.Join(parent, "LICENSE")
			}
			if err != nil || got != want {
				t.Fatalf("got (%q, %v), want %q", got, err, want)
			}
		})
	}
}
