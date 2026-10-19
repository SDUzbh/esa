package codex

import (
	"testing"
	"time"
)

func TestMavenDownloadsAreLimitedToTheCutoffOnlyInReplays(t *testing.T) {
	cutoff := time.Date(2016, 3, 1, 0, 0, 0, 0, time.UTC)
	if (&Provider{}).mavenPolicy(cutoff) != nil {
		t.Fatal("downloads enabled without a repository")
	}
	production := (&Provider{MavenRepository: "https://repo1.maven.org/maven2"}).mavenPolicy(cutoff)
	if production == nil || production.Repository != "https://repo1.maven.org/maven2" || production.PublishedBefore != nil {
		t.Fatalf("production downloads must have no publication limit: %+v", production)
	}
	replay := (&Provider{MavenRepository: "https://repo1.maven.org/maven2", MavenBeforeCutoff: true}).mavenPolicy(cutoff)
	if replay == nil || replay.PublishedBefore == nil || !replay.PublishedBefore.Equal(cutoff) {
		t.Fatalf("a replay must refuse artifacts published after its cutoff: %+v", replay)
	}
	if child := (&Provider{MavenRepository: "https://r", MavenBeforeCutoff: true}).child(); child.MavenRepository != "https://r" || !child.MavenBeforeCutoff {
		t.Fatal("registry tasks run in a child provider, which must keep the download policy")
	}
}
