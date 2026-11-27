package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/javimartin/pi-autopilot/internal/semver"
)

const (
	defaultConfigFile = "semver-config.yaml"
)

type SemVerConfig struct {
	AutoTag        bool   `yaml:"auto_tag"`
	TagName        string `yaml:"tag_name"`
	GenerateNotes  bool   `yaml:"generate_notes"`
	ReleaseNotesDir string `yaml:"release_notes_dir"`
}

func main() {
	flag.Parse()

	gitDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current directory: %v", err)
	}

	vm, err := semver.New(gitDir)
	if err != nil {
		log.Fatalf("Failed to initialize version manager: %v", err)
	}
	defer vm.ReleaseLock()

	fmt.Printf("Current working directory: %s\n", gitDir)
	currentVer := vm.GetVersion()
	fmt.Printf("Current version: %s\n", currentVer.String())

	newCommits, err := vm.GetCommitsSinceLastTag()
	if err != nil {
		log.Fatalf("Failed to get commits: %v", err)
	}

	if len(newCommits) == 0 {
		fmt.Println("No new commits since last release. Exiting.")
		os.Exit(0)
	}

	fmt.Printf("Found %d new commits since last release\n", len(newCommits))
	for i, commit := range newCommits {
		if i < 10 {
			fmt.Printf("  [%d] %s: %s\n", i+1, commit.Type, commit.Subject)
		}
		if i == 9 {
			fmt.Printf("  ... and %d more\n", len(newCommits)-10)
		}
	}

	newVersion, err := vm.BumpVersion()
	if err != nil {
		log.Fatalf("Failed to calculate new version: %v", err)
	}

	fmt.Printf("\nNew version: %s\n", newVersion.String())

	previousVersion := vm.GetPreviousVersion()
	if previousVersion.Equal(semver.SemVer{}) {
		fmt.Println("Bump type: initial release (no previous version)")
	} else {
		bumpType := "patch"
		if newVersion.Major != previousVersion.Major {
			bumpType = "major"
		} else if newVersion.Minor != previousVersion.Minor {
			bumpType = "minor"
		}
		fmt.Printf("Bump type: %s\n", bumpType)
	}

	// Generate release notes
	if vm.GetVersion().Equal(previousVersion) {
		// No version change, no notes needed
	} else {
		notes, err := vm.BuildReleaseNotes(previousVersion, newVersion)
		if err != nil {
			log.Printf("Warning: Failed to build release notes: %v", err)
		} else {
			fmt.Println("\nRelease Notes:")
			fmt.Println(strings.Repeat("=", 80))
			fmt.Println(notes)
			fmt.Println(strings.Repeat("=", 80))

			// Save release notes file
			releaseDir := ".github/releases"
			if err := os.MkdirAll(releaseDir, 0755); err != nil {
				log.Printf("Warning: Failed to create releases directory: %v", err)
			} else {
				releaseFile := fmt.Sprintf("%s/%s.md", releaseDir, newVersion.String())
				if err := os.WriteFile(releaseFile, []byte(notes), 0644); err != nil {
					log.Printf("Warning: Failed to write release notes to %s: %v", releaseFile, err)
				} else {
					fmt.Printf("Release notes written to %s\n", releaseFile)
				}
			}
		}
	}

	fmt.Println("\n✓ Version bump complete!")
	fmt.Printf("  New version: %s\n", newVersion.String())
}
