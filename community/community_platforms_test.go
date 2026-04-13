package community_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// CommunityPlatform mirrors the structure in community_platforms.json.
type CommunityPlatform struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Emoji       string `json:"emoji"`
	Description string `json:"description"`
	URL         string `json:"url"`
	BadgeFlat   string `json:"badge_flat"`
	BadgeLarge  string `json:"badge_large"`
}

// repoRoot returns the absolute path to the repository root
// (two levels up from this test file's directory).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine test file path")
	}
	// This file lives at <repo>/community/community_platforms_test.go
	return filepath.Dir(filepath.Dir(filename))
}

// loadPlatforms reads and parses community_platforms.json from the repo root.
func loadPlatforms(t *testing.T) []CommunityPlatform {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "community_platforms.json"))
	if err != nil {
		t.Fatalf("failed to read community_platforms.json: %v", err)
	}
	var platforms []CommunityPlatform
	if err := json.Unmarshal(data, &platforms); err != nil {
		t.Fatalf("failed to parse community_platforms.json: %v", err)
	}
	return platforms
}

// TestCommunityPlatformsCount ensures exactly 6 official platforms are defined.
func TestCommunityPlatformsCount(t *testing.T) {
	platforms := loadPlatforms(t)
	if len(platforms) != 6 {
		t.Errorf("expected exactly 6 community platforms, got %d", len(platforms))
	}
}

// TestCommunityPlatformsOrder ensures platforms are in the canonical order.
func TestCommunityPlatformsOrder(t *testing.T) {
	expectedOrder := []string{"slack", "github", "youtube", "substack", "twitter", "blog"}
	platforms := loadPlatforms(t)

	ids := make([]string, len(platforms))
	for i, p := range platforms {
		ids[i] = p.id()
	}

	if len(ids) != len(expectedOrder) {
		t.Fatalf("platform count mismatch: got %v, want %v", ids, expectedOrder)
	}
	for i, id := range ids {
		if id != expectedOrder[i] {
			t.Errorf("platform[%d]: got %q, want %q", i, id, expectedOrder[i])
		}
	}
}

func (p CommunityPlatform) id() string { return p.ID }

// TestCommunityPlatformsRequiredFields checks that every platform has all fields populated.
func TestCommunityPlatformsRequiredFields(t *testing.T) {
	for _, p := range loadPlatforms(t) {
		if p.ID == "" {
			t.Error("platform has empty id")
		}
		if p.Name == "" {
			t.Errorf("platform %q has empty name", p.ID)
		}
		if p.Emoji == "" {
			t.Errorf("platform %q has empty emoji", p.ID)
		}
		if p.Description == "" {
			t.Errorf("platform %q has empty description", p.ID)
		}
		if p.URL == "" {
			t.Errorf("platform %q has empty url", p.ID)
		}
	}
}

// TestLinkedInNotPresent ensures LinkedIn is permanently excluded.
func TestLinkedInNotPresent(t *testing.T) {
	for _, p := range loadPlatforms(t) {
		lower := strings.ToLower(p.Name) + strings.ToLower(p.ID) + strings.ToLower(p.URL)
		if strings.Contains(lower, "linkedin") {
			t.Errorf("LinkedIn must not be in community platforms, found in %q", p.ID)
		}
	}
}

// TestNoCorruptedEmoji scans key Markdown files for the UTF-8 replacement character (U+FFFD).
func TestNoCorruptedEmoji(t *testing.T) {
	root := repoRoot(t)
	files := []string{
		"README.md",
		"README-UnitGen.md",
		"READMEes-Es.md",
		"READMEfr-FR.md",
		"READMEja-JP.md",
		"HACKTOBERFEST_GUIDE.md",
	}

	for _, name := range files {
		path := filepath.Join(root, name)
		f, err := os.Open(path)
		if err != nil {
			t.Errorf("failed to open %s: %v", name, err)
			continue
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			if strings.Contains(scanner.Text(), "\uFFFD") {
				t.Errorf("%s:%d contains corrupted emoji (U+FFFD replacement character)", name, lineNum)
			}
		}
	}
}

// TestReadmeContainsAllPlatformURLs verifies that each README's community section
// references every canonical platform URL from community_platforms.json.
func TestReadmeContainsAllPlatformURLs(t *testing.T) {
	root := repoRoot(t)
	platforms := loadPlatforms(t)

	files := []string{
		"README.md",
		"README-UnitGen.md",
		"READMEes-Es.md",
		"READMEfr-FR.md",
		"READMEja-JP.md",
		"HACKTOBERFEST_GUIDE.md",
	}

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("failed to read %s: %v", name, err)
			continue
		}
		content := string(data)
		for _, p := range platforms {
			if !strings.Contains(content, p.URL) {
				t.Errorf("%s is missing URL for platform %q: %s", name, p.ID, p.URL)
			}
		}
	}
}

// TestReadmeDoesNotContainLinkedIn verifies LinkedIn is absent from all README files.
func TestReadmeDoesNotContainLinkedIn(t *testing.T) {
	root := repoRoot(t)
	files := []string{
		"README.md",
		"README-UnitGen.md",
		"READMEes-Es.md",
		"READMEfr-FR.md",
		"READMEja-JP.md",
		"HACKTOBERFEST_GUIDE.md",
	}

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("failed to read %s: %v", name, err)
			continue
		}
		content := strings.ToLower(string(data))
		if strings.Contains(content, "linkedin.com/company/keploy") {
			t.Errorf("%s still contains a LinkedIn community link — LinkedIn must be removed", name)
		}
	}
}
