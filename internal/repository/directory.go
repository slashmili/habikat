package repository

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/slashmili/habikat/internal/git"
)

func SourceDirectory(ctx context.Context, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root, err := (git.Local{Dir: canonical}).Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		absent, markerErr := repositoryAbsent(canonical, err)
		if markerErr != nil {
			return "", markerErr
		}
		if absent {
			return canonical, nil
		}
		return "", fmt.Errorf("discover source directory: %w", err)
	}
	return filepath.EvalSymlinks(strings.TrimSuffix(string(root), "\n"))
}
