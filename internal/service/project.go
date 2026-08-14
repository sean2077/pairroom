package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type gitRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execGitRunner struct{}

func (execGitRunner) Run(ctx context.Context, directory string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		detail := strings.TrimSpace(string(exitErr.Stderr))
		if detail != "" {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
		}
	}
	return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

type ProjectResolver struct {
	git gitRunner
}

func NewProjectResolver() *ProjectResolver {
	return &ProjectResolver{git: execGitRunner{}}
}

func newProjectResolverForTest(runner gitRunner) *ProjectResolver {
	return &ProjectResolver{git: runner}
}

func (r *ProjectResolver) Resolve(ctx context.Context, input string) (Project, error) {
	if r == nil || r.git == nil {
		return Project{}, errors.New("project resolver is not configured")
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return Project{}, errors.New("project path is required")
	}
	if !filepath.IsAbs(input) {
		return Project{}, errors.New("project path must be absolute")
	}
	absolute, err := filepath.Abs(input)
	if err != nil {
		return Project{}, fmt.Errorf("resolve absolute project path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return Project{}, fmt.Errorf("resolve project symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Project{}, fmt.Errorf("stat project path: %w", err)
	}
	if !info.IsDir() {
		return Project{}, errors.New("project path must be a directory")
	}
	output, err := r.git.Run(ctx, resolved, "rev-parse", "--show-toplevel")
	if err != nil {
		return Project{}, fmt.Errorf("project path is not an accessible Git working tree: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return Project{}, errors.New("git returned an empty worktree root")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(resolved, root)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Project{}, fmt.Errorf("resolve Git worktree root: %w", err)
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return Project{}, fmt.Errorf("resolve Git worktree root symlinks: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return Project{}, fmt.Errorf("stat Git worktree root: %w", err)
	}
	if !rootInfo.IsDir() {
		return Project{}, errors.New("Git worktree root is not a directory")
	}
	return Project{ID: projectID(root), Root: root, Available: true}, nil
}

func projectID(root string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(root)))
	return "project-" + hex.EncodeToString(digest[:12])
}
