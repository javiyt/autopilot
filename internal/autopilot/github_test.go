package autopilot

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func ghArgFixture(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"git", "gh", "pi"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := testConfig(dir)
	cfg.Repo = "HuddleScope/huddlescope.github.io"
	a, err := NewApp(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestGhArgsPositionalRepoCommands(t *testing.T) {
	a := ghArgFixture(t)
	got, err := a.ghArgs("repo", "view", "--json", "nameWithOwner,defaultBranchRef")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"repo", "view", "HuddleScope/huddlescope.github.io", "--json", "nameWithOwner,defaultBranchRef"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repo view must take the repository positionally: %v", got)
	}
}

func TestGhArgsAppendsFlagForOtherCommands(t *testing.T) {
	a := ghArgFixture(t)
	got, err := a.ghArgs("issue", "list", "--state", "open")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"issue", "list", "--state", "open", "--repo", "HuddleScope/huddlescope.github.io"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flag form expected for other subcommands: %v", got)
	}
}

func TestGhArgsRequiresConfiguredRepo(t *testing.T) {
	a := ghArgFixture(t)
	a.cfg.Repo = ""
	if _, err := a.ghArgs("repo", "view", "--json", "name"); err == nil {
		t.Fatal("expected an error for an empty configured repository")
	}
}
