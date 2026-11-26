package workspacereview

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectMaterialsSnapshotDigestsCompleteTruncatedUntrackedText(t *testing.T) {
	workspace := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "review@example.invalid"}, {"config", "user.name", "Review Test"}, {"config", "commit.gpgsign", "false"}} {
		command := exec.Command("git", args...)
		command.Dir = workspace
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	path := filepath.Join(workspace, "large.txt")
	content := func(tail byte) []byte {
		// The prefix limit falls on the first byte of the three-byte rune. The
		// truncation boundary must be treated as partial UTF-8, not binary.
		return []byte(strings.Repeat("a", int(maxUntrackedFileBytes)-1) + "界" + fmt.Sprintf("tail-%c", tail))
	}
	if err := os.WriteFile(path, content('a'), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CollectMaterialsSnapshot(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || len(first.Digest) != 64 || !strings.Contains(first.Text, "truncated: first 1024 KiB shown") {
		t.Fatalf("first snapshot changed=%t digest=%q text marker=%t", first.Changed, first.Digest, strings.Contains(first.Text, "truncated: first 1024 KiB shown"))
	}
	if err := os.WriteFile(path, content('b'), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := CollectMaterialsSnapshot(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("digest did not change when content beyond the review prefix changed")
	}
}
