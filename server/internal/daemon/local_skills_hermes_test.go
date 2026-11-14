package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestHermesAgentSkillInventoryUsesSelectedProfileAndAgent(t *testing.T) {
	home := t.TempDir()
	hermesHome := filepath.Join(home, ".hermes")
	profile := filepath.Join(hermesHome, "profiles", "research")
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", hermesHome)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	writeTestLocalSkill(t, filepath.Join(hermesHome, "skills"), "default-only", map[string]string{
		"SKILL.md": "# Default skill",
	})
	writeTestLocalSkill(t, filepath.Join(profile, "skills"), "profile-only", map[string]string{
		"SKILL.md":                "# Profile skill",
		"references/checklist.md": "Check the build",
	})
	if err := os.WriteFile(filepath.Join(profile, "config.yaml"), []byte("skills:\n  external_dirs: [extra-skills]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestLocalSkill(t, filepath.Join(profile, "extra-skills"), "external-only", map[string]string{
		"SKILL.md": "# Profile external skill",
	})
	d := &Daemon{logger: slog.Default()}
	for _, id := range []string{"agent-a", "agent-b"} {
		store := execenv.HermesAgentSkillStorePath("", id, profile)
		writeTestLocalSkill(t, store, id+"-learned", map[string]string{
			"SKILL.md": "# Learned skill",
		})
	}
	rt := Runtime{ID: "runtime", Provider: "hermes"}
	roots, supported, err := d.hermesAgentSkillRoots(rt, protocol.HermesSkillInventoryAgent{
		ID: "agent-a", Args: []string{"--profile", "research"},
	})
	if err != nil || !supported {
		t.Fatalf("list skills: supported=%v err=%v", supported, err)
	}
	skills, _, err := listLocalSkillsInRoots(rt.Provider, roots)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"profile-only":    localSkillRootHermesProfile,
		"external-only":   localSkillRootHermesProfile,
		"agent-a-learned": localSkillRootHermesAgent,
	}
	for _, skill := range skills {
		if want[skill.Key] != skill.Root {
			t.Errorf("unexpected skill: %+v", skill)
		}
		delete(want, skill.Key)
	}
	if len(want) != 0 {
		t.Errorf("missing skills: %v", want)
	}
	preview, err := readHermesSkillPreview(roots, "profile-only")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Content != "# Profile skill" || len(preview.Files) != 1 || preview.Files[0].Content != "Check the build" {
		t.Fatalf("unexpected skill preview: %+v", preview)
	}
	// A supporting file linked outside the skill stays out of the preview.
	if err := os.Symlink(filepath.Join(profile, "config.yaml"),
		filepath.Join(profile, "skills", "profile-only", "linked-config.yaml")); err == nil {
		preview, err := readHermesSkillPreview(roots, "profile-only")
		if err != nil || len(preview.Files) != 1 {
			t.Fatalf("preview with linked file: %+v, %v", preview, err)
		}
	}

	for _, key := range []string{"agent-b-learned", "../profile-only", "profile-only/../external-only"} {
		if _, err := readHermesSkillPreview(roots, key); err == nil {
			t.Errorf("unexpected preview for %q", key)
		}
	}

}
