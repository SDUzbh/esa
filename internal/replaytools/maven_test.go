package replaytools

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mavenRepository serves one artifact like Maven Central: the JAR with its
// publication time, its SHA-1 and a POM with dependencies.
func mavenRepository(t *testing.T, jar []byte, published time.Time, sha string) string {
	t.Helper()
	if sha == "" {
		sum := sha1.Sum(jar)
		sha = hex.EncodeToString(sum[:])
	}
	pom := `<project xmlns="http://maven.apache.org/POM/4.0.0"><version>1.3.0</version>
<properties><xxhash.version>0.5</xxhash.version></properties><dependencies>
<dependency><groupId>org.example</groupId><artifactId>xxhash</artifactId><version>${xxhash.version}</version></dependency>
<dependency><groupId>org.example</groupId><artifactId>managed</artifactId></dependency>
<dependency><groupId>junit</groupId><artifactId>junit</artifactId><version>4.12</version><scope>test</scope></dependency>
<dependency><groupId>org.example</groupId><artifactId>optional</artifactId><version>1</version><optional>true</optional></dependency>
</dependencies></project>`
	base := "/net/jpountz/lz4/lz4/1.3.0/lz4-1.3.0"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case base + ".jar":
			w.Header().Set("Last-Modified", published.UTC().Format(http.TimeFormat))
			w.Write(jar)
		case base + ".jar.sha1":
			w.Write([]byte(sha + "  lz4-1.3.0.jar\n"))
		case base + ".pom":
			w.Write([]byte(pom))
		case base + ".pom.sha1":
			sum := sha1.Sum([]byte(pom))
			w.Write([]byte(hex.EncodeToString(sum[:])))
		case "/moved/a/1/a-1.jar":
			// Followed, this would fetch the real JAR and then fail on a missing SHA-1.
			http.Redirect(w, r, base+".jar", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	previous := mavenTransport
	mavenTransport = server.Client().Transport
	t.Cleanup(func() { mavenTransport = previous })
	return server.URL
}

func mavenFixture(t *testing.T, repository string, before *time.Time) Server {
	t.Helper()
	installDocker(t)
	profiles, _ := filepath.Abs("../../profiles")
	s := callFixture(t)
	s.Config.Container, s.Config.ProfilesRoot = "task", profiles
	s.Config.Maven = &MavenPolicy{Repository: repository, PublishedBefore: before}
	s.granted = []string{"temporary_tools"}
	return s
}

func install(s *Server, coordinate string) (map[string]any, error) {
	args, _ := json.Marshal(map[string]string{"coordinate": coordinate, "reason": "compile the frozen client",
		"hypothesis_id": "H1", "expected_observation": "the client compiles"})
	return s.Call(context.Background(), "maven_install", args)
}

func TestMavenInstallDownloadsAnyArtifactOnDemand(t *testing.T) {
	jar := []byte("PK lz4 classes")
	s := mavenFixture(t, mavenRepository(t, jar, time.Date(2014, 11, 26, 0, 0, 0, 0, time.UTC), ""), nil)
	result, err := install(&s, "net.jpountz.lz4:lz4:1.3.0")
	if err != nil || result["status"] != "installed" || result["path"] != "/workspace/tools/maven/net.jpountz.lz4/lz4-1.3.0.jar" {
		t.Fatal("artifact not installed", result, err)
	}
	if deps := result["dependencies"].([]string); len(deps) != 1 || deps[0] != "org.example:xxhash:0.5" {
		t.Fatalf("runtime dependencies (test and optional ones excluded): %v", deps)
	}
	if unresolved := result["unresolved_dependencies"].([]string); len(unresolved) != 1 || !strings.Contains(unresolved[0], "managed") {
		t.Fatalf("dependencies whose version needs a parent: %v", unresolved)
	}
	if result["published"] != "2014-11-26T00:00:00Z" {
		t.Fatal(result["published"])
	}
	if again, err := install(&s, "net.jpountz.lz4:lz4:1.3.0"); err != nil || again["cache_hit"] != true {
		t.Fatal("an installed artifact with matching bytes was not reused", again, err)
	}
}

func TestMavenInstallReplayCutoffAndIntegrity(t *testing.T) {
	published := time.Date(2014, 11, 26, 0, 0, 0, 0, time.UTC)
	before, after := published.Add(-time.Hour), published.Add(time.Hour)
	// A replay whose ticket predates the artifact cannot install it.
	s := mavenFixture(t, mavenRepository(t, []byte("jar"), published, ""), &before)
	if result, err := install(&s, "net.jpountz.lz4:lz4:1.3.0"); err != nil || result["status"] != "denied" ||
		!strings.Contains(result["reason"].(string), "after this replay's observation cutoff") {
		t.Fatal("a post-cutoff artifact was installed", result, err)
	}
	s = mavenFixture(t, mavenRepository(t, []byte("jar"), published, ""), &after)
	if result, err := install(&s, "net.jpountz.lz4:lz4:1.3.0"); err != nil || result["status"] != "installed" {
		t.Fatal("an artifact published before the cutoff was refused", result, err)
	}
	s = mavenFixture(t, mavenRepository(t, []byte("jar"), published, strings.Repeat("0", 40)), nil)
	if result, _ := install(&s, "net.jpountz.lz4:lz4:1.3.0"); result["status"] != "failed" || !strings.Contains(result["reason"].(string), "SHA-1") {
		t.Fatal("a JAR that does not match the repository's SHA-1 was installed", result)
	}
	if result, _ := install(&s, "moved:a:1"); result["status"] != "failed" || !strings.Contains(result["reason"].(string), "download failed") {
		t.Fatal("a redirect was followed", result)
	}
}

func TestMavenInstallStaysInsidePolicy(t *testing.T) {
	s := mavenFixture(t, mavenRepository(t, []byte("jar"), time.Now(), ""), nil)
	for coordinate, want := range map[string]string{
		"net.jpountz.lz4:lz4":          "coordinate",
		"../../etc:passwd:1":           "coordinate",
		"net.jpountz.lz4:lz4:1.3.0/..": "coordinate",
		"a:b:c:d":                      "coordinate",
	} {
		if result, err := install(&s, coordinate); err != nil || result["status"] != "denied" || !strings.Contains(result["reason"].(string), want) {
			t.Errorf("%s: %v %v", coordinate, result, err)
		}
	}
	s.granted = nil
	if result, _ := install(&s, "net.jpountz.lz4:lz4:1.3.0"); result["status"] != "denied" || !strings.Contains(result["reason"].(string), "temporary_tools") {
		t.Fatal("installed without the temporary_tools grant", result)
	}
	s.Config.Maven = nil
	if result, _ := install(&s, "net.jpountz.lz4:lz4:1.3.0"); result["status"] != "denied" || !strings.Contains(result["reason"].(string), "not enabled") {
		t.Fatal("installed with downloads disabled", result)
	}
	s.Config.Maven = &MavenPolicy{Repository: "http://repo.invalid"}
	s.granted = []string{"temporary_tools"}
	if result, _ := install(&s, "net.jpountz.lz4:lz4:1.3.0"); result["status"] != "failed" || !strings.Contains(result["reason"].(string), "HTTPS") {
		t.Fatal("a plain-HTTP repository was used", result)
	}
	audit, err := os.ReadFile(s.Config.AuditPath)
	if err != nil || strings.Count(string(audit), `"tool":"maven_install"`) != 7 {
		t.Fatal("maven_install calls were not all audited", err)
	}
}

func TestMavenToolIsListedOnlyWhenEnabled(t *testing.T) {
	listed := func(s Server) bool {
		in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
		var out strings.Builder
		_ = s.Serve(context.Background(), in, &out)
		return strings.Contains(out.String(), `"maven_install"`)
	}
	profiles, _ := filepath.Abs("../../profiles")
	s := callFixture(t)
	s.Config.ProfilesRoot = profiles
	if listed(s) {
		t.Fatal("listed with downloads disabled")
	}
	s.Config.Maven = &MavenPolicy{Repository: "https://repo1.maven.org/maven2"}
	if !listed(s) {
		t.Fatal("not listed with downloads enabled")
	}
}
