package semver

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type SemanticVersionManager struct {
	gitDir    string
	tagsLock  sync.RWMutex
	tags      map[string]SemVer
	current   SemVer
	locked    bool
}

func New(gitDir string) (*SemanticVersionManager, error) {
	gitDir = gitDir
	if gitDir == "" {
		gitDir = "."
	}
	toplevel, err := exec.Command("git", "-C", gitDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	gitDir = strings.TrimSpace(string(toplevel))

	vm := &SemanticVersionManager{
		gitDir: gitDir,
		tags:   make(map[string]SemVer),
	}

	if err := vm.ReloadTags(); err != nil {
		return nil, err
	}

	vm.current = SemVer{0, 0, 0}
	for _, v := range vm.tags {
		if !v.Equal(SemVer{0, 0, 0}) {
			vm.current = v
			break
		}
	}

	return vm, nil
}

func (vm *SemanticVersionManager) ReloadTags() error {
	vm.tagsLock.Lock()
	defer vm.tagsLock.Unlock()
	tags, err := GetTags(vm.gitDir)
	if err != nil {
		return err
	}
	vm.tags = make(map[string]SemVer)
	for _, tag := range tags {
		v, err := TagVersion(tag)
		if err != nil {
			continue
		}
		vm.tags[tag] = v
	}
	return nil
}

func (vm *SemanticVersionManager) GetVersion() SemVer {
	vm.tagsLock.RLock()
	defer vm.tagsLock.RUnlock()
	return vm.current
}

func (vm *SemanticVersionManager) BumpVersion() (SemVer, error) {
	current := vm.GetVersion()
	commits, err := vm.GetCommitsSinceLastTag()
	if err != nil {
		return current, fmt.Errorf("failed to get commits: %w", err)
	}
	if len(commits) == 0 {
		return current, nil
	}
	newVersion := CalculateVersion(commits, current)
	return newVersion, nil
}

func (vm *SemanticVersionManager) GetCommitsSinceLastTag() ([]ConventionalCommit, error) {
	vm.tagsLock.RLock()
	defer vm.tagsLock.RUnlock()
	previousVersion := vm.GetPreviousVersion()
	return GetCommitsSinceTag(vm.gitDir, previousVersion.String())
}

func (vm *SemanticVersionManager) GetPreviousVersion() SemVer {
	vm.tagsLock.RLock()
	defer vm.tagsLock.RUnlock()
	tags := vm.sortTags()
	if len(tags) == 0 {
		return SemVer{0, 0, 0}
	}
	return vm.tags[tags[0]]
}

func (vm *SemanticVersionManager) sortTags() []string {
	type tv struct {
		Tag      string
		Version  SemVer
	}
	var tagged []tv
	for tag, version := range vm.tags {
		tagged = append(tagged, tv{tag, version})
	}
	sort.Slice(tagged, func(i, j int) bool {
		return tagged[i].Version.Less(tagged[j].Version)
	})
	results := make([]string, len(tagged))
	for i, t := range tagged {
		results[i] = t.Tag
	}
	return results
}

func (vm *SemanticVersionManager) CreateTag(version SemVer) error {
	tag := "v" + version.String()
	vm.tagsLock.Lock()
	defer vm.tagsLock.Unlock()
	if vm.hasTag(tag) {
		return fmt.Errorf("tag %q already exists", tag)
	}
	cmd := exec.Command("git", "tag", tag)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tag: %w", err)
	}
	vm.tags[tag] = version
	vm.current = version
	return nil
}

func (vm *SemanticVersionManager) CreateTagOnCommit(version SemVer, sha string) error {
	tag := "v" + version.String()
	vm.tagsLock.Lock()
	defer vm.tagsLock.Unlock()
	if vm.hasTag(tag) {
		return fmt.Errorf("tag %q already exists", tag)
	}
	cmd := exec.Command("git", "tag", "-f", tag, sha)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tag: %w", err)
	}
	vm.tags[tag] = version
	vm.current = version
	return nil
}

func (vm *SemanticVersionManager) hasTag(tag string) bool {
	vm.tagsLock.RLock()
	defer vm.tagsLock.RUnlock()
	_, exists := vm.tags[tag]
	return exists
}

func (vm *SemanticVersionManager) BuildReleaseNotes(fromVersion, toVersion SemVer) (string, error) {
	commits, err := GetCommitsSinceTag(vm.gitDir, fromVersion.String())
	if err != nil {
		return "", err
	}
	notes := BuildReleaseNotes(commits)
	return ReleaseNotes(notes, toVersion.String()), nil
}

func (vm *SemanticVersionManager) CreateTagAndNotes(newVersion SemVer, sha string) (string, error) {
	previousVersion := vm.GetPreviousVersion()
	tag := "v" + newVersion.String()
	vm.tagsLock.RLock()
	tagExists := vm.hasTag(tag)
	vm.tagsLock.RUnlock()
	if tagExists {
		return "", fmt.Errorf("tag %q already exists, skipping", tag)
	}
	if err := vm.CreateTagOnCommit(newVersion, sha); err != nil {
		return "", err
	}
	notes, err := vm.BuildReleaseNotes(previousVersion, newVersion)
	if err != nil {
		return notes, err
	}
	return notes, nil
}

func (vm *SemanticVersionManager) Bump() (string, error) {
	previousVersion := vm.GetPreviousVersion()
	newVersion := vm.GetVersion()
	newVersion = CalculateVersion(nil, newVersion)
	bumpType := "patch"
	if newVersion.Major != previousVersion.Major {
		bumpType = "major"
	} else if newVersion.Minor != previousVersion.Minor {
		bumpType = "minor"
	}
	return bumpType, nil
}

func (vm *SemanticVersionManager) IsLocked() bool {
	vm.tagsLock.RLock()
	defer vm.tagsLock.RUnlock()
	return vm.locked
}

func (vm *SemanticVersionManager) AcquireLock() error {
	vm.tagsLock.Lock()
	defer vm.tagsLock.Unlock()
	if vm.locked {
		return fmt.Errorf("version calculation is already locked")
	}
	vm.locked = true
	return nil
}

func (vm *SemanticVersionManager) ReleaseLock() {
	vm.tagsLock.Lock()
	defer vm.tagsLock.Unlock()
	vm.locked = false
}

func (vm *SemanticVersionManager) WaitForLock(timeout time.Duration) error {
	for time.Since(time.Now()) < timeout {
		vm.tagsLock.RLock()
		if !vm.locked {
			vm.tagsLock.RUnlock()
			return nil
		}
		vm.tagsLock.RUnlock()
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for lock")
}
