//go:build linux

package reviewer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceCleanupPreservesConcurrentCanonicalReplacements(t *testing.T) {
	for _, mode := range []string{"socket-before-retire", "directory-before-retire", "directory-after-retire"} {
		t.Run(mode, func(t *testing.T) {
			path := t.TempDir()
			job := filepath.Join(path, "job-identity")
			if err := os.Mkdir(job, 0700); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(job, "source.sock")
			if err := os.WriteFile(socket, []byte("owned socket inode"), 0600); err != nil {
				t.Fatal(err)
			}
			directory, _ := os.Lstat(job)
			inode, _ := os.Lstat(socket)
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			retained := socket
			err = cleanupSourceEndpoint(root, filepath.Base(job), directory, inode, func(stage string) {
				if mode == "socket-before-retire" && stage == "before-retire" {
					// Keep the original inode allocated. Unlinking a closed file
					// lets ext4 immediately reuse it for the replacement, which
					// would no longer exercise a distinct-identity replacement.
					if err := os.Rename(socket, filepath.Join(path, "retained-original.sock")); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(socket, []byte("replacement-canary"), 0600); err != nil {
						t.Fatal(err)
					}
					replacement, err := os.Lstat(socket)
					if err != nil || os.SameFile(inode, replacement) {
						t.Fatal("replacement fixture must have a distinct inode", err)
					}
				}
				if mode == "directory-before-retire" && stage == "before-retire" {
					if err := os.Rename(job, job+"-retained-original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(job, 0700); err != nil {
						t.Fatal(err)
					}
					retained = filepath.Join(job, "canary")
					if err := os.WriteFile(retained, []byte("replacement-canary"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "directory-after-retire" && stage == "after-retire" {
					if err := os.Mkdir(job, 0700); err != nil {
						t.Fatal(err)
					}
					retained = filepath.Join(job, "canary")
					if err := os.WriteFile(retained, []byte("replacement-canary"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
			if mode != "directory-after-retire" && err == nil {
				t.Fatal("unknown retired inode treated as owned")
			}
			if mode == "directory-after-retire" && err != nil {
				t.Fatal("owned isolated cleanup failed", err)
			}
			data, err := os.ReadFile(retained)
			if err != nil || string(data) != "replacement-canary" {
				t.Fatal("concurrent replacement lost", err)
			}
		})
	}
}
