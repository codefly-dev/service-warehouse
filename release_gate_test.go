package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnpublishedImageExceptionEndsAtFirstLockOrRelease(t *testing.T) {
	script, err := filepath.Abs(".github/scripts/allow-unpublished-image.sh")
	require.NoError(t, err)
	for _, scenario := range []string{"unpublished", "previous lock", "release tag", "shallow history"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				command := exec.Command("git", args...)
				command.Dir = dir
				output, err := command.CombinedOutput()
				require.NoError(t, err, "%s", output)
				return string(output)
			}
			git("init", "--quiet")
			git("config", "commit.gpgsign", "false")
			git("config", "tag.gpgsign", "false")
			git("config", "core.hooksPath", "/dev/null")
			git("config", "user.name", "gate fixture")
			git("config", "user.email", "fixture@example.invalid")
			commit := func(digest string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway-image.json"), []byte(`{"name":"registry.example/warehouse","digest":"`+digest+`"}`), 0600))
				git("add", "gateway-image.json")
				git("commit", "--quiet", "-m", "fixture")
			}
			commit("")
			switch scenario {
			case "previous lock":
				commit("sha256:" + strings.Repeat("a", 64))
				commit("")
			case "release tag":
				git("tag", "v0.0.2") // This tag exists only in the disposable test repository.
			case "shallow history":
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(git("rev-parse", "HEAD")), 0600))
			}
			command := exec.Command("bash", script)
			command.Dir = dir
			output, err := command.CombinedOutput()
			if scenario == "unpublished" {
				require.NoError(t, err, "%s", output)
			} else {
				require.Error(t, err, "%s", output)
			}
		})
	}
}
