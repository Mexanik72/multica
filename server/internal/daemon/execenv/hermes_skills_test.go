package execenv

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHermesAgentSkillStoreAcrossRuns(t *testing.T) {
	source := t.TempDir()
	store := filepath.Join(t.TempDir(), "agent-a", "skills")
	otherStore := filepath.Join(t.TempDir(), "agent-b", "skills")
	if err := os.WriteFile(filepath.Join(source, "config.yaml"), []byte("skills: {create_dir: original}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepare := func(store string) string {
		t.Helper()
		home := t.TempDir()
		if _, err := prepareHermesHome(home, source, false, nil, nil, "", "", store, testLogger()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Skills struct {
				CreateDir string `yaml:"create_dir"`
			}
		}
		if err := yaml.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if got := hermesExternalDirs(t, filepath.Join(home, "config.yaml")); len(got) != 2 || got[0] != filepath.Join(source, "skills") || got[1] != filepath.Join(source, "original") {
			t.Fatalf("skills.external_dirs = %v", got)
		}
		if config.Skills.CreateDir != store {
			t.Fatalf("skills.create_dir = %q, want %q", config.Skills.CreateDir, store)
		}
		return config.Skills.CreateDir
	}
	path := filepath.Join(prepare(store), "test-skill", "SKILL.md")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("agent skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(store string) ([]byte, error) {
		return os.ReadFile(filepath.Join(prepare(store), "test-skill", "SKILL.md"))
	}
	if got, err := read(store); err != nil || string(got) != "agent skill" {
		t.Fatalf("next run skill = %q, %v", got, err)
	}
	if _, err := read(otherStore); !os.IsNotExist(err) {
		t.Fatalf("another agent sees the skill: %v", err)
	}
}

func TestPersistHermesSessionSkills(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(t.TempDir(), "agent-skills")
	source := t.TempDir()
	assigned := []SkillContextForEnv{{Name: "Assigned", Content: "Assigned skill"}}
	if _, err := prepareHermesHome(home, source, false, assigned, nil, "", "", store, testLogger()); err != nil {
		t.Fatal(err)
	}

	for path, content := range map[string]string{
		"learned/review/SKILL.md":                "Learned skill",
		"learned/review/references/checklist.md": "Check the build",
		"bundled/example/SKILL.md":               "Bundled skill",
		".bundled_manifest":                      "example:hash\n",
	} {
		fullPath := filepath.Join(home, "skills", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PersistHermesSessionSkills(home, store, assigned); err != nil {
		t.Fatal(err)
	}
	saved := HermesSessionSkillStore(store)
	if content, err := os.ReadFile(filepath.Join(saved, "learned", "review", "references", "checklist.md")); err != nil || string(content) != "Check the build" {
		t.Fatalf("saved reference = %q, %v", content, err)
	}
	for _, key := range []string{"assigned", "bundled"} {
		if _, err := os.Stat(filepath.Join(saved, key)); !os.IsNotExist(err) {
			t.Fatalf("unexpected saved entry %q: %v", key, err)
		}
	}
	next := t.TempDir()
	if _, err := prepareHermesHome(next, source, false, nil, nil, "", "", store, testLogger()); err != nil {
		t.Fatal(err)
	}
	if dirs := hermesExternalDirs(t, filepath.Join(next, "config.yaml")); len(dirs) == 0 || dirs[0] != saved {
		t.Fatalf("next run does not load saved skills: %v", dirs)
	}
}
