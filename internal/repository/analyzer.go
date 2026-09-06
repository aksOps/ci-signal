package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Options struct {
	RepositoryDir     string
	GitPath           string
	ASTGrepPath       string
	GoRulePath        string
	MaxMetadataBytes  int
	MaxASTSourceBytes int64
	MaxASTOutputBytes int
	GeneratedSuffixes []string
	ExcludedDirs      []string
	ExcludedPaths     []string
}

type Analyzer struct {
	repositoryDir     string
	repositoryReal    string
	gitPath           string
	astGrepPath       string
	goRulePath        string
	maxMetadataBytes  int
	maxASTSourceBytes int64
	maxASTOutputBytes int
	generatedSuffixes []string
	excludedDirs      map[string]struct{}
	excludedPaths     []string
}

func NewAnalyzer(options Options) (*Analyzer, error) {
	if !filepath.IsAbs(options.RepositoryDir) {
		return nil, errors.New("repository directory must be absolute")
	}
	real, err := filepath.EvalSymlinks(options.RepositoryDir)
	if err != nil {
		return nil, fmt.Errorf("resolve repository directory: %w", err)
	}
	if options.GitPath == "" || options.ASTGrepPath == "" || !filepath.IsAbs(options.GoRulePath) {
		return nil, errors.New("Git, ast-grep, and absolute Go rule paths are required")
	}
	if options.MaxMetadataBytes < 1 || options.MaxASTSourceBytes < 1 || options.MaxASTOutputBytes < 1 {
		return nil, errors.New("repository analysis limits must be positive")
	}
	if _, err := os.Stat(options.GoRulePath); err != nil {
		return nil, fmt.Errorf("stat Go ast-grep rule: %w", err)
	}
	excluded := map[string]struct{}{".git": {}, "vendor": {}, "node_modules": {}}
	for _, directory := range options.ExcludedDirs {
		if directory == "" || strings.Contains(directory, "/") {
			return nil, fmt.Errorf("excluded directory %q must be one path segment", directory)
		}
		excluded[directory] = struct{}{}
	}
	for _, excluded := range options.ExcludedPaths {
		if _, err := literalPath(excluded); err != nil {
			return nil, fmt.Errorf("invalid excluded path: %w", err)
		}
	}
	suffixes := append([]string{".gen.go", ".generated.go", ".pb.go"}, options.GeneratedSuffixes...)
	return &Analyzer{
		repositoryDir:     filepath.Clean(options.RepositoryDir),
		repositoryReal:    filepath.Clean(real),
		gitPath:           options.GitPath,
		astGrepPath:       options.ASTGrepPath,
		goRulePath:        options.GoRulePath,
		maxMetadataBytes:  options.MaxMetadataBytes,
		maxASTSourceBytes: options.MaxASTSourceBytes,
		maxASTOutputBytes: options.MaxASTOutputBytes,
		generatedSuffixes: suffixes,
		excludedDirs:      excluded,
		excludedPaths:     append([]string(nil), options.ExcludedPaths...),
	}, nil
}

func (a *Analyzer) gitArgs(arguments ...string) []string {
	prefix := []string{"--no-pager", "-c", "safe.directory=" + a.repositoryReal, "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "-c", "core.attributesFile=/dev/null"}
	return append(prefix, arguments...)
}

func (a *Analyzer) runGit(ctx context.Context, limit int, arguments ...string) ([]byte, error) {
	return a.run(ctx, a.gitPath, a.gitArgs(arguments...), limit)
}

func (a *Analyzer) run(ctx context.Context, executable string, arguments []string, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stdout limitedBuffer
	stdout.limit = limit
	var stderr limitedBuffer
	stderr.limit = 32 * 1024
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = a.repositoryDir
	command.Env = safeChildEnvironment()
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if errors.Is(stdout.err, ErrOutputLimit) {
		return nil, ErrOutputLimit
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s failed: %s", filepath.Base(executable), message)
	}
	return stdout.Bytes(), nil
}

func (a *Analyzer) runGitChunk(ctx context.Context, offset int64, limit int, arguments ...string) (Chunk, error) {
	if offset < 0 || limit < 1 {
		return Chunk{}, errors.New("chunk offset and limit are invalid")
	}
	command := exec.CommandContext(ctx, a.gitPath, a.gitArgs(arguments...)...)
	command.Dir = a.repositoryDir
	command.Env = safeChildEnvironment()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Chunk{}, err
	}
	var stderr limitedBuffer
	stderr.limit = 32 * 1024
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return Chunk{}, err
	}
	if offset > 0 {
		copied, copyErr := io.CopyN(io.Discard, stdout, offset)
		if copyErr != nil {
			waitErr := command.Wait()
			if errors.Is(copyErr, io.EOF) && waitErr == nil {
				return Chunk{Offset: copied, Complete: true, TotalBytes: copied}, nil
			}
			return Chunk{}, fmt.Errorf("seek command output: %w", copyErr)
		}
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(limit)+1))
	if readErr != nil {
		command.Process.Kill()
		command.Wait()
		return Chunk{}, fmt.Errorf("read command output: %w", readErr)
	}
	if len(data) > limit {
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return Chunk{}, fmt.Errorf("stop bounded command: %w", err)
		}
		command.Wait()
		return Chunk{Data: data[:limit], Offset: offset, NextOffset: offset + int64(limit), Complete: false}, nil
	}
	if err := command.Wait(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return Chunk{}, fmt.Errorf("git failed: %s", message)
	}
	return Chunk{Data: data, Offset: offset, Complete: true, TotalBytes: offset + int64(len(data))}, nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
	err    error
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.err = ErrOutputLimit
		return 0, b.err
	}
	if len(value) > remaining {
		b.buffer.Write(value[:remaining])
		b.err = ErrOutputLimit
		return remaining, b.err
	}
	return b.buffer.Write(value)
}

func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }

func safeChildEnvironment() []string {
	allowed := map[string]struct{}{"PATH": {}, "LANG": {}, "LC_ALL": {}, "TMPDIR": {}, "SYSTEMROOT": {}}
	result := make([]string, 0, len(allowed)+2)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, keep := allowed[key]; keep {
			result = append(result, item)
		}
	}
	return append(result, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}
