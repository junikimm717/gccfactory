package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const attemptsPerURL = 3

// A mirror that accepts the connection and then goes silent would otherwise
// hang forever: ResponseHeaderTimeout covers the headers, nothing covers the
// body. config.sub is a dependency of every srctree, so one stalled mirror can
// stall the whole build.
const stallTimeout = 90 * time.Second

func backoff(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second }

var client = &http.Client{
	// No overall timeout: gcc-14.2.0.tar.xz is ~90 MB and slow mirrors exist.
	// Progress is bounded by the dial and response-header timeouts instead.
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 60 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   60 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		MaxIdleConns:          16,
	},
}

func CacheDir(distDir string) string { return filepath.Join(distDir, "src") }

// Path embeds the checksum prefix so a re-pin never collides with a stale
// file.
func Path(distDir string, s Source) string {
	return filepath.Join(CacheDir(distDir), s.SHA256[:16]+"-"+s.File)
}

func stampPath(distDir string, s Source) string { return Path(distDir, s) + ".done" }
func lockPath(distDir string, s Source) string {
	return filepath.Join(CacheDir(distDir), "."+s.SHA256[:16]+".lock")
}

// It is idempotent and safe against concurrent processes: the download lands in
// a temp file in the same directory, is verified, and is then os.Rename'd into
// place while holding an exclusive flock on a per-source lock file.
func Fetch(ctx context.Context, distDir string, s Source) (string, error) {
	if err := Validate([]Source{s}); err != nil {
		return "", err
	}
	dir := CacheDir(distDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sources: creating %s: %w", dir, err)
	}
	dst := Path(distDir, s)

	// Fast path: a .done stamp means some process already verified this exact
	// content, so we do not re-hash a 90 MB tarball on every build.
	if verified(distDir, s) {
		return dst, nil
	}

	unlock, err := lockExclusive(ctx, lockPath(distDir, s))
	if err != nil {
		return "", err
	}
	defer unlock()

	// Re-check: another process may have finished while we waited.
	if verified(distDir, s) {
		return dst, nil
	}
	// The file may exist without a stamp (older run, or a stamp was deleted).
	// Hash it; if it matches, just stamp it.
	if sum, err := hashFile(dst); err == nil {
		if sum == s.SHA256 {
			return dst, stamp(distDir, s)
		}
		if err := os.Remove(dst); err != nil {
			return "", fmt.Errorf("sources: removing corrupt %s: %w", dst, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	var problems []string
	for _, u := range s.URLs {
		for attempt := 0; attempt < attemptsPerURL; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(backoff(attempt)):
				}
			}
			err := download(ctx, u, dst, s)
			if err == nil {
				return dst, stamp(distDir, s)
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			problems = append(problems, fmt.Sprintf("  %s (try %d): %v", u, attempt+1, err))
			// A bad checksum or a 4xx will not fix itself; move to the next
			// mirror instead of burning the backoff.
			var mm *ChecksumError
			var pe *permanentError
			if errors.As(err, &mm) || errors.As(err, &pe) {
				break
			}
		}
	}
	return "", fmt.Errorf("sources: could not fetch %s from any of %d mirrors:\n%s",
		s.File, len(s.URLs), strings.Join(problems, "\n"))
}

// permanentError marks a failure that a retry cannot fix.
type permanentError struct{ error }

func (e *permanentError) Unwrap() error { return e.error }

// ChecksumError reports a body whose hash did not match the pin.
type ChecksumError struct {
	Source Source
	URL    string
	Actual string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("sha256 mismatch for %s from %s: expected %s, got %s",
		e.Source.File, e.URL, e.Source.SHA256, e.Actual)
}

func download(ctx context.Context, url, dst string, s Source) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gccfactory/1 (+sources)")
	resp, err := client.Do(req) // http.Client follows redirects by default
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("HTTP %s", resp.Status)
		if c := resp.StatusCode; c >= 400 && c < 500 && c != 408 && c != 429 {
			return &permanentError{err} // retrying a 404 only wastes time
		}
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+s.File+".part-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	var stalled atomic.Bool
	timer := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancel() })
	defer timer.Stop()
	body := &stallReader{r: resp.Body, timer: timer}
	if _, err = io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		if stalled.Load() {
			return fmt.Errorf("stalled: no data for %s", stallTimeout)
		}
		return fmt.Errorf("reading body: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != s.SHA256 {
		err = &ChecksumError{Source: s, URL: url, Actual: got}
		return err
	}
	if err = os.Chmod(tmpName, 0o444); err != nil {
		return err
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("publishing %s: %w", dst, err)
	}
	return nil
}

func verified(distDir string, s Source) bool {
	if _, err := os.Stat(stampPath(distDir, s)); err != nil {
		return false
	}
	fi, err := os.Stat(Path(distDir, s))
	return err == nil && fi.Mode().IsRegular()
}

func stamp(distDir string, s Source) error {
	p := stampPath(distDir, s)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("sources: writing stamp %s: %w", p, err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s  %s\n", s.SHA256, s.File)
	return err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sources: hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Lock files are never removed: deleting one would break flock identity for a
// process already holding it.
func lockExclusive(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("sources: opening lock %s: %w", path, err)
	}
	// Flock blocks in the kernel, so run it off-thread to stay ctx-cancellable.
	done := make(chan error, 1)
	go func() { done <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-done:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("sources: flock %s: %w", path, err)
		}
	case <-ctx.Done():
		// The goroutine will finish and its lock dies with the fd we close.
		go func() { <-done; f.Close() }()
		return nil, ctx.Err()
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// stallReader restarts the stall timer on every successful read, so the
// deadline measures silence rather than total transfer time -- a slow mirror
// serving a 90 MB tarball is fine, a mute one is not.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallTimeout)
	}
	return n, err
}
