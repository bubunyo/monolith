package monolith

import (
	"os"
	"path/filepath"
)

// CachePath returns a path under the monolith root, ~/.monolith.
// It does not create anything.
func CachePath(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home, ".monolith"}, elem...)...), nil
}
