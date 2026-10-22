package replaytools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MavenPolicy lets the Agent install any Maven artifact on demand. The tool
// server, on the host, downloads it and copies it into the offline sandbox;
// the sandbox itself never reaches the network.
type MavenPolicy struct {
	// Repository is the HTTPS base URL, normally Maven Central.
	Repository string `json:"repository"`
	// PublishedBefore, set only for historical replays, refuses artifacts the
	// repository published after the task's observation cutoff.
	PublishedBefore *time.Time `json:"published_before,omitempty"`
	// Cache, when set, is a host directory that keeps verified downloads so
	// each artifact is fetched from the repository once for all tasks.
	Cache string `json:"cache,omitempty"`
	// CacheMaxBytes caps the cache; the least recently used files go first.
	CacheMaxBytes int64 `json:"cache_max_bytes,omitempty"`
}

// mavenTransport is the HTTP transport for repository downloads; tests
// replace it to trust a local TLS server.
var mavenTransport http.RoundTripper = http.DefaultTransport

// mavenMaxBytes bounds one downloaded JAR.
const mavenMaxBytes = 64 << 20

var mavenPart = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+\-]*$`)

// pomProperty matches a ${property} reference in a POM value.
var pomProperty = regexp.MustCompile(`\$\{([^}]+)\}`)

// mavenCoordinate splits group:artifact:version and rejects anything that
// could leave the repository's path layout.
func mavenCoordinate(s string) (group, artifact, version string, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		if !mavenPart.MatchString(p) || strings.Contains(p, "..") {
			return "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], true
}

func mavenTool() map[string]any {
	str := map[string]string{"type": "string"}
	return map[string]any{
		"name": "maven_install",
		"description": "Download any Maven Central artifact (group:artifact:version) on the host, verify Maven's SHA-1 and install the JAR into " +
			"/workspace/tools/maven. Requires a granted temporary_tools capability. Returns the artifact's runtime dependencies to install next.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"coordinate": str, "reason": str, "hypothesis_id": str, "expected_observation": str,
			},
			"required":             []string{"coordinate", "reason", "hypothesis_id", "expected_observation"},
			"additionalProperties": false,
		},
	}
}

// mavenUserAgent identifies the provider to the repository, which asks
// automated clients to name themselves.
const mavenUserAgent = "esa-engineering-provider/1 (Go)"

// mavenRetryBase is the first wait after a throttled or failed request; tests shorten it.
var mavenRetryBase = 2 * time.Second

// fetch downloads one repository file; redirects are refused. Throttling
// (HTTP 429) and server errors are retried up to four times with exponential
// backoff, honoring Retry-After, at most 30 seconds per wait.
func (p MavenPolicy) fetch(ctx context.Context, path string, limit int64) ([]byte, http.Header, error) {
	client := &http.Client{Transport: mavenTransport, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("repository redirects are not permitted")
	}}
	var response *http.Response
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.Repository, "/")+"/"+path, nil)
		if err != nil {
			return nil, nil, err
		}
		request.Header.Set("User-Agent", mavenUserAgent)
		response, err = client.Do(request)
		if err != nil {
			return nil, nil, errors.New("repository download failed")
		}
		if (response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500) || attempt == 4 {
			break
		}
		wait := mavenRetryBase << attempt
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
			wait = time.Duration(seconds) * time.Second
		}
		wait = min(wait, 30*time.Second)
		response.Body.Close()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response.Header, fmt.Errorf("repository returned HTTP %d for %s", response.StatusCode, path)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s exceeds %d MiB", path, limit>>20)
	}
	return data, response.Header, err
}

func (s *Server) installMaven(ctx context.Context, coordinate string) (map[string]any, error) {
	call, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	policy := *s.Config.Maven
	if !strings.HasPrefix(policy.Repository, "https://") {
		return nil, errors.New("Maven downloads require an HTTPS repository")
	}
	group, artifact, version, _ := mavenCoordinate(coordinate)
	base := strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + version + "/" + artifact + "-" + version
	data, published, err := policy.verified(call, base+".jar", mavenMaxBytes)
	if err != nil {
		return nil, err
	}
	if policy.PublishedBefore != nil && (published.IsZero() || published.After(*policy.PublishedBefore)) {
		when := "at an unknown time"
		if !published.IsZero() {
			when = published.UTC().Format(time.RFC3339)
		}
		return map[string]any{"status": "denied", "reason": fmt.Sprintf("%s was published %s, after this replay's observation cutoff %s",
			coordinate, when, policy.PublishedBefore.UTC().Format(time.RFC3339))}, nil
	}
	digest := sha256.Sum256(data)
	dest := "/workspace/tools/maven/" + group + "/" + artifact + "-" + version + ".jar"
	result, err := s.placeTool(call, dest, data, hex.EncodeToString(digest[:]), version)
	if err != nil {
		return nil, err
	}
	result["coordinate"] = coordinate
	if !published.IsZero() {
		result["published"] = published.UTC().Format(time.RFC3339)
	}
	if pom, _, err := policy.verified(call, base+".pom", 1<<20); err == nil {
		result["dependencies"], result["unresolved_dependencies"] = pomDependencies(pom)
	}
	return result, nil
}

// pomDependencies lists a POM's compile and runtime dependencies. Versions
// that need a parent POM or dependency management are reported unresolved.
func pomDependencies(data []byte) ([]string, []string) {
	var pom struct {
		Version string `xml:"version"`
		Parent  struct {
			Version string `xml:"version"`
		} `xml:"parent"`
		Properties struct {
			Values []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"properties"`
		Dependencies []struct {
			Group    string `xml:"groupId"`
			Artifact string `xml:"artifactId"`
			Version  string `xml:"version"`
			Scope    string `xml:"scope"`
			Optional string `xml:"optional"`
			Type     string `xml:"type"`
		} `xml:"dependencies>dependency"`
	}
	if xml.Unmarshal(data, &pom) != nil {
		return nil, nil
	}
	props := map[string]string{"project.version": pom.Version, "version": pom.Version}
	if pom.Version == "" {
		props["project.version"], props["version"] = pom.Parent.Version, pom.Parent.Version
	}
	for _, p := range pom.Properties.Values {
		props[p.XMLName.Local] = strings.TrimSpace(p.Value)
	}
	resolve := func(s string) string {
		return pomProperty.ReplaceAllStringFunc(strings.TrimSpace(s), func(m string) string {
			if v, ok := props[m[2:len(m)-1]]; ok && v != "" {
				return v
			}
			return m
		})
	}
	resolved, unresolved := []string{}, []string{}
	for _, d := range pom.Dependencies {
		if (d.Scope != "" && d.Scope != "compile" && d.Scope != "runtime") || d.Optional == "true" || (d.Type != "" && d.Type != "jar") {
			continue
		}
		c := resolve(d.Group) + ":" + resolve(d.Artifact) + ":" + resolve(d.Version)
		if _, _, _, ok := mavenCoordinate(c); ok {
			resolved = append(resolved, c)
		} else {
			unresolved = append(unresolved, c)
		}
	}
	return resolved, unresolved
}
