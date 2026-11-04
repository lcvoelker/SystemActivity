package storage

import (
	"crypto/sha256"
	"io"
	"os"
)

// fingerprint returns the SHA-256 and size of a regular file, streaming it
// so large files aren't loaded into memory. ok is false for directories and
// files that can't be read (e.g. already deleted or locked).
func fingerprint(path string) (sum []byte, size int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, false
	}

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, false
	}
	return h.Sum(nil), n, true
}
