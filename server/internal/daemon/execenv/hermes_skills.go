package execenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/skill"
	"gopkg.in/yaml.v3"
)

// HermesAgentSkillStorePath is scoped to the agent and the selected Hermes
// profile. Its lifetime is independent of any task or conversation.
func HermesAgentSkillStorePath(daemonProfile, agentID, sourceHome string) string {
	agentID = sanitizePathSegment(agentID)
	if agentID == "" {
		return ""
	}
	profile, err := cli.ProfileDir(daemonProfile)
	if err != nil {
		return ""
	}
	return filepath.Join(profile, "hermes-skills", agentID, hermesMemoryProfileSegment(sourceHome))
}

// HermesProfileSkillDirs resolves the profile's local skills and configured
// skill roots using the same expansion as managed tasks.
func HermesProfileSkillDirs(home string, env map[string]string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read Hermes profile config: %w", err)
	}
	var doc yaml.Node
	if err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse Hermes profile config: %w", err)
		}
	}
	return hermesProfileSkillDirs(home, &doc, env), nil
}

func hermesProfileSkillDirs(home string, doc *yaml.Node, env map[string]string) []string {
	dirs := existingHermesExternalDirs(doc)
	createDir := resolveHermesYAMLAlias(yamlMapValue(yamlMapValue(yamlDocumentRoot(doc), "skills"), "create_dir"))
	if createDir != nil && createDir.Kind == yaml.ScalarNode && createDir.Tag == "!!str" {
		dirs = append([]string{createDir.Value}, dirs...)
	}
	profileSkills := filepath.Join(home, "skills")
	dirs = computeHermesExternalDirs(home, dirs, env)
	dirs = slices.DeleteFunc(dirs, func(dir string) bool { return dir == profileSkills })
	return append([]string{profileSkills}, dirs...)
}

// HermesSessionSkillStore holds skills created in the session-local skills/
// directory by Hermes versions that do not support skills.create_dir.
func HermesSessionSkillStore(store string) string {
	return filepath.Join(store, ".session-skills")
}

// PersistHermesSessionSkills publishes new session-local skills before the
// next run rebuilds the session's skills directory.
func PersistHermesSessionSkills(home, store string, assigned []SkillContextForEnv) error {
	local := filepath.Join(home, "skills")
	excluded := make(map[string]bool, len(assigned))
	for _, name := range resolveSkillSlugs(assigned) {
		excluded[name] = true
	}
	manifest, err := os.ReadFile(filepath.Join(local, ".bundled_manifest"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for line := range strings.SplitSeq(string(manifest), "\n") {
		name, _, _ := strings.Cut(line, ":")
		excluded[strings.TrimSpace(name)] = true
	}
	var saveErrors []error
	err = filepath.WalkDir(local, func(dir string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || dir == local {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") || excluded[entry.Name()] {
			return filepath.SkipDir
		}
		main := filepath.Join(dir, "SKILL.md")
		info, err := os.Lstat(main)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return filepath.SkipDir
		}
		content, err := os.ReadFile(main)
		if err != nil {
			return err
		}
		name, _ := skill.ParseSkillFrontmatter(string(content))
		if !excluded[name] || name == "" {
			relative, err := filepath.Rel(local, dir)
			if err != nil {
				return err
			}
			if err := publishHermesSessionSkill(dir, filepath.Join(HermesSessionSkillStore(store), relative)); err != nil {
				saveErrors = append(saveErrors, fmt.Errorf("save Hermes skill %q: %w", relative, err))
			}
		}
		return filepath.SkipDir
	})
	return errors.Join(append(saveErrors, err)...)
}

func publishHermesSessionSkill(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	staging, err := newHermesStoreStaging(destination)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	bundle, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer bundle.Close()
	if err := fs.WalkDir(bundle.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported skill file %q", path)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := os.CopyFS(staging, bundle.FS()); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		// Concurrent conversations can create the same skill. The first
		// complete bundle wins.
		if _, statErr := os.Stat(filepath.Join(destination, "SKILL.md")); statErr != nil {
			return err
		}
	}
	return nil
}
