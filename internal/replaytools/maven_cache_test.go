package replaytools

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingRepository serves files with SHA-1s and publication times and counts requests.
func countingRepository(t *testing.T, files map[string]string, published time.Time) (string, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("User-Agent") != mavenUserAgent {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if body, ok := files[name]; ok {
			w.Header().Set("Last-Modified", published.UTC().Format(http.TimeFormat))
			w.Write([]byte(body))
			return
		}
		if body, ok := files[strings.TrimSuffix(name, ".sha1")]; ok && strings.HasSuffix(name, ".sha1") {
			if sum, bad := files[name+".bad"]; bad {
				w.Write([]byte(sum))
				return
			}
			sum := sha1.Sum([]byte(body))
			w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + filepath.Base(name)))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	transport := mavenTransport
	mavenTransport = server.Client().Transport
	t.Cleanup(func() { mavenTransport = transport })
	return server.URL, &requests
}

func TestThrottledDownloadsAreRetried(t *testing.T) {
	previous := mavenRetryBase
	mavenRetryBase = time.Millisecond
	t.Cleanup(func() { mavenRetryBase = previous })
	var attempts atomic.Int32
	throttle := atomic.Int32{}
	throttle.Store(2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("User-Agent") != mavenUserAgent {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		if throttle.Add(-1) >= 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("payload"))
	}))
	defer server.Close()
	transport := mavenTransport
	mavenTransport = server.Client().Transport
	defer func() { mavenTransport = transport }()
	policy := MavenPolicy{Repository: server.URL}
	if data, _, err := policy.fetch(context.Background(), "a/b.jar", 1<<10); err != nil || string(data) != "payload" || attempts.Load() != 3 {
		t.Fatalf("%q %v after %d attempts", data, err, attempts.Load())
	}
	throttle.Store(100)
	attempts.Store(0)
	if _, _, err := policy.fetch(context.Background(), "a/b.jar", 1<<10); err == nil || !strings.Contains(err.Error(), "429") || attempts.Load() != 5 {
		t.Fatalf("persistent throttling must give up after 5 attempts with the status: %v (%d)", err, attempts.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mavenRetryBase = time.Hour
	if _, _, err := policy.fetch(ctx, "a/b.jar", 1<<10); err == nil {
		t.Fatal("a cancelled task must not wait out the backoff")
	}
}

func TestEachArtifactIsDownloadedOnce(t *testing.T) {
	published := time.Date(2014, 11, 26, 20, 40, 9, 0, time.UTC)
	url, requests := countingRepository(t, map[string]string{"g/a/1/a-1.jar": "JAR BYTES"}, published)
	policy := MavenPolicy{Repository: url, Cache: t.TempDir()}
	for i := 0; i < 3; i++ {
		data, when, err := policy.verified(context.Background(), "g/a/1/a-1.jar", 1<<10)
		if err != nil || string(data) != "JAR BYTES" || !when.Equal(published) {
			t.Fatalf("read %d: %q %v %v", i, data, when, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("%d requests for one artifact read three times; want 2 (file and SHA-1)", requests.Load())
	}
	// Another repository never shares these files.
	other := MavenPolicy{Repository: url + "/mirror", Cache: policy.Cache}
	if other.cachePath("g/a/1/a-1.jar") == policy.cachePath("g/a/1/a-1.jar") {
		t.Fatal("repositories share a cache entry")
	}
}

func TestDamagedOrUnverifiedFilesAreNotServedFromTheCache(t *testing.T) {
	url, requests := countingRepository(t, map[string]string{"g/a/1/a-1.jar": "GOOD", "g/b/1/b-1.jar": "TAMPERED", "g/b/1/b-1.jar.sha1.bad": "0000"}, time.Now())
	policy := MavenPolicy{Repository: url, Cache: t.TempDir()}
	if _, _, err := policy.verified(context.Background(), "g/a/1/a-1.jar", 1<<10); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy.cachePath("g/a/1/a-1.jar"), []byte("CORRUPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _, err := policy.verified(context.Background(), "g/a/1/a-1.jar", 1<<10)
	if err != nil || string(data) != "GOOD" || requests.Load() != 4 {
		t.Fatalf("a damaged cache entry was served or not refetched: %q %v %d", data, err, requests.Load())
	}
	if cached, _ := os.ReadFile(policy.cachePath("g/a/1/a-1.jar")); string(cached) != "GOOD" {
		t.Fatal("the repaired file was not cached again")
	}
	if _, _, err := policy.verified(context.Background(), "g/b/1/b-1.jar", 1<<10); err == nil || !strings.Contains(err.Error(), "SHA-1") {
		t.Fatal("a file failing its SHA-1 was accepted", err)
	}
	if _, err := os.Stat(policy.cachePath("g/b/1/b-1.jar")); !os.IsNotExist(err) {
		t.Fatal("a file failing its SHA-1 was cached")
	}
}

func TestTheReplayDateLimitAppliesToCachedArtifacts(t *testing.T) {
	published := time.Date(2014, 11, 26, 0, 0, 0, 0, time.UTC)
	url := mavenRepository(t, []byte("jar"), published, "")
	cache := t.TempDir()
	open := mavenFixture(t, url, nil)
	open.Config.Maven.Cache = cache
	if result, err := install(&open, "net.jpountz.lz4:lz4:1.3.0"); err != nil || result["status"] != "installed" {
		t.Fatal(result, err)
	}
	before := published.Add(-time.Hour)
	replay := mavenFixture(t, url, &before)
	replay.Config.Maven.Cache = cache
	if result, err := install(&replay, "net.jpountz.lz4:lz4:1.3.0"); err != nil || result["status"] != "denied" ||
		!strings.Contains(result["reason"].(string), "2014-11-26T00:00:00Z") {
		t.Fatal("a cached artifact bypassed the replay's cutoff", result, err)
	}
}

func TestTheCacheKeepsItsSizeCap(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		files["g/"+name+"/1/"+name+"-1.jar"] = strings.Repeat(name, 10)
	}
	url, _ := countingRepository(t, files, time.Now())
	policy := MavenPolicy{Repository: url, Cache: t.TempDir(), CacheMaxBytes: 25}
	read := func(name string) {
		t.Helper()
		if _, _, err := policy.verified(context.Background(), "g/"+name+"/1/"+name+"-1.jar", 1<<10); err != nil {
			t.Fatal(err)
		}
	}
	read("a")
	read("b")
	past := time.Now().Add(-time.Hour)
	os.Chtimes(policy.cachePath("g/b/1/b-1.jar"), past, past) // b is least recently used
	read("a")                                                 // a is touched
	read("c")                                                 // 30 bytes > 25: evict b
	exists := func(name string) bool {
		_, err := os.Stat(policy.cachePath("g/" + name + "/1/" + name + "-1.jar"))
		return err == nil
	}
	if !exists("a") || exists("b") || !exists("c") {
		t.Fatalf("eviction kept a=%v b=%v c=%v", exists("a"), exists("b"), exists("c"))
	}
	if _, err := os.Stat(policy.cachePath("g/b/1/b-1.jar") + ".sha1"); !os.IsNotExist(err) {
		t.Fatal("an evicted file left its checksum behind")
	}
}

func TestConcurrentTasksShareTheCache(t *testing.T) {
	url, _ := countingRepository(t, map[string]string{"g/a/1/a-1.jar": strings.Repeat("x", 4096)}, time.Now())
	policy := MavenPolicy{Repository: url, Cache: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if data, _, err := policy.verified(context.Background(), "g/a/1/a-1.jar", 1<<20); err != nil || len(data) != 4096 {
				t.Error(len(data), err)
			}
		}()
	}
	wg.Wait()
	if data, _, ok := policy.fromCache("g/a/1/a-1.jar"); !ok || len(data) != 4096 {
		t.Fatal("concurrent writers left an invalid cache entry")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(policy.cachePath("g/a/1/a-1.jar")), ".partial-*"))
	if len(leftovers) != 0 {
		t.Fatal("temporary files were left behind", leftovers)
	}
}
