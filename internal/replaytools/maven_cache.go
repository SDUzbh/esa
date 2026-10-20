package replaytools

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The host-side cache keeps verified repository files so each artifact is
// downloaded once for all tasks, as Maven Central asks of heavy automated
// users. A file is stored only after its SHA-1 matches the repository's, is
// checked again on every read, and is written atomically so concurrent tool
// servers can share the directory. Its publication time is kept with it, so a
// replay's date limit applies to cached files exactly as to downloads.

// mavenCacheDefaultBytes caps the cache when the policy names no limit.
const mavenCacheDefaultBytes = 2 << 30

// cachePath is where a repository file lives in the cache, separated per repository.
func (p MavenPolicy) cachePath(rel string) string {
	key := sha256.Sum256([]byte(strings.TrimRight(p.Repository, "/")))
	return filepath.Join(p.Cache, hex.EncodeToString(key[:8]), filepath.FromSlash(rel))
}

// verified returns a repository file checked against its .sha1, from the
// cache when it holds a valid copy, else downloaded and then cached.
func (p MavenPolicy) verified(ctx context.Context, rel string, limit int64) (data []byte, published time.Time, err error) {
	if p.Cache != "" {
		if data, published, ok := p.fromCache(rel); ok {
			return data, published, nil
		}
	}
	data, header, err := p.fetch(ctx, rel, limit)
	if err != nil {
		return nil, published, err
	}
	expected, _, err := p.fetch(ctx, rel+".sha1", 1<<10)
	if err != nil {
		return nil, published, err
	}
	if !sha1Matches(data, expected) {
		return nil, published, errors.New("downloaded file does not match the repository's SHA-1: " + rel)
	}
	published, _ = http.ParseTime(header.Get("Last-Modified"))
	if p.Cache != "" {
		// A cache that cannot be written only costs a later download.
		_ = p.store(rel, data, expected, published)
	}
	return data, published, nil
}

func sha1Matches(data, expected []byte) bool {
	sum := sha1.Sum(data)
	fields := strings.Fields(string(expected))
	return len(fields) > 0 && strings.EqualFold(fields[0], hex.EncodeToString(sum[:]))
}

func (p MavenPolicy) fromCache(rel string) ([]byte, time.Time, bool) {
	path := p.cachePath(rel)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	expected, err := os.ReadFile(path + ".sha1")
	if err != nil || !sha1Matches(data, expected) {
		// Damaged or partial: drop it and download again.
		removeCached(path)
		return nil, time.Time{}, false
	}
	var published time.Time
	if stamp, err := os.ReadFile(path + ".published"); err == nil {
		published, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(stamp)))
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now) // recently used files are evicted last
	return data, published, true
}

func (p MavenPolicy) store(rel string, data, expected []byte, published time.Time) error {
	path := p.cachePath(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	stamp := ""
	if !published.IsZero() {
		stamp = published.UTC().Format(time.RFC3339)
	}
	// The checksum and time land before the file, so a reader never sees the
	// file without them; a half-written set fails verification and is dropped.
	for _, part := range []struct {
		suffix string
		data   []byte
	}{{".sha1", expected}, {".published", []byte(stamp)}, {"", data}} {
		if err := writeAtomic(path+part.suffix, part.data); err != nil {
			return err
		}
	}
	return p.evict()
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func removeCached(path string) {
	for _, suffix := range []string{"", ".sha1", ".published"} {
		os.Remove(path + suffix)
	}
}

// evict removes the least recently used files until the cache fits its cap.
func (p MavenPolicy) evict() error {
	limit := p.CacheMaxBytes
	if limit <= 0 {
		limit = mavenCacheDefaultBytes
	}
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var entries []entry
	var total int64
	err := filepath.WalkDir(p.Cache, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".sha1") || strings.HasSuffix(path, ".published") ||
			strings.HasPrefix(d.Name(), ".partial-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil || total <= limit {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	for _, e := range entries {
		if total <= limit {
			break
		}
		removeCached(e.path)
		total -= e.size
	}
	return nil
}
