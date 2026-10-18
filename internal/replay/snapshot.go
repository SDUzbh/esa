package replay

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"esa/internal/esa"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Plan struct {
	Ticket  esa.Ticket `json:"ticket"`
	Audit   Audit      `json:"audit"`
	Cutoff  time.Time  `json:"cutoff"`
	Ref     string     `json:"source_ref"`
	Commit  string     `json:"source_commit"`
	RawHash string     `json:"raw_sha256"`
}

// Resolve accepts only an unambiguous affected version or an explicit historical
// ref. Current release metadata is never used to infer an unreleased build.
func Resolve(ctx context.Context, rawPath, repo, ref, cutoff, project string) (Plan, error) {
	var p Plan
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		return p, err
	}
	if cutoff == "" {
		var x struct {
			Issue struct {
				Fields struct {
					Created string `json:"created"`
				} `json:"fields"`
			} `json:"issue"`
		}
		if err = json.Unmarshal(raw, &x); err != nil {
			return p, err
		}
		p.Cutoff, err = esa.ParseTime(x.Issue.Fields.Created)
	} else {
		p.Cutoff, err = time.Parse(time.RFC3339Nano, cutoff)
	}
	if err != nil || p.Cutoff.IsZero() {
		return p, fmt.Errorf("valid historical cutoff is required")
	}
	p.Ticket, p.Audit, err = JiraAt(raw, project, p.Cutoff)
	if err != nil {
		return p, err
	}
	if ref == "" {
		if len(p.Ticket.Versions) != 1 {
			return p, fmt.Errorf("version selection needs clarification: %v", p.Ticket.Versions)
		}
		ref = "refs/tags/" + p.Ticket.Versions[0]
	}
	p.Ref = strings.TrimPrefix(ref, "refs/tags/")
	git := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
	}
	b, err := git("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return p, fmt.Errorf("version has no verified local Git ref: %s", ref)
	}
	p.Commit = strings.TrimSpace(string(b))
	b, err = git("show", "-s", "--format=%aI%n%cI", p.Commit)
	if err != nil {
		return p, err
	}
	for _, ts := range strings.Fields(string(b)) {
		t, e := time.Parse(time.RFC3339, ts)
		if e != nil || t.After(p.Cutoff) {
			return p, fmt.Errorf("source commit is after cutoff; supply the actual historical development ref")
		}
	}
	h := sha256.Sum256(raw)
	p.RawHash = hex.EncodeToString(h[:])
	return p, nil
}

// Export streams the archive. Failure removes only this newly created bundle.
func Export(ctx context.Context, p Plan, repo, out, scope string) (err error) {
	if out == "" {
		return fmt.Errorf("output directory is required")
	}
	if scope != "clients" && scope != "full" {
		return fmt.Errorf("scope must be clients or full")
	}
	if _, e := os.Stat(out); !os.IsNotExist(e) {
		return fmt.Errorf("output already exists; use a new bundle directory")
	}
	if err = os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(out)
		}
	}()
	args := []string{"-C", repo, "archive", "--format=tar", p.Commit}
	if scope == "clients" {
		args = append(args, "clients/src/main/java", "clients/src/test/java/org/apache/kafka/clients/MockClient.java", "LICENSE", "NOTICE", "build.gradle")
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(child, "git", args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	waited := false
	defer func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	hashes := map[string]string{}
	reader := tar.NewReader(pipe)
	for {
		header, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("non-regular archive entry rejected: %s", header.Name)
		}
		name := filepath.Clean(header.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe archive path")
		}
		path := filepath.Join(out, "source", name)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if e != nil {
			return e
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		hashes[name] = hex.EncodeToString(hash.Sum(nil))
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		return err
	}
	manifest := map[string]any{
		"source_key":             p.Ticket.SourceKey,
		"project_id":             p.Ticket.ProjectID,
		"cutoff":                 p.Cutoff,
		"source_ref":             p.Ref,
		"source_commit":          p.Commit,
		"raw_sha256":             p.RawHash,
		"audit":                  p.Audit,
		"files":                  hashes,
		"scope":                  scope,
		"environment_provenance": "Reported fields or explicit operator supplement; customer OS/JDK are not inferred.",
	}
	for name, value := range map[string]any{"ticket.json": p.Ticket, "manifest.json": manifest} {
		b, e := json.MarshalIndent(value, "", "  ")
		if e != nil {
			return e
		}
		if err = os.WriteFile(filepath.Join(out, name), append(b, '\n'), 0600); err != nil {
			return err
		}
	}
	return nil
}
