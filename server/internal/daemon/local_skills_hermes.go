package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func resolveHermesLaunchProfile(home string, fixedArgs, customArgs []string, logger *slog.Logger) execenv.HermesProfileResolution {
	selection := agent.ParseHermesProfileArgs(agent.HermesLaunchArgv(fixedArgs, customArgs, logger))
	return execenv.ResolveHermesProfile(home, selection.Name, selection.Found, selection.Inline)
}

func (d *Daemon) handleHermesAgentSkillList(ctx context.Context, rt Runtime, pending PendingLocalSkills) {
	target := *pending.HermesAgent
	roots, supported, err := d.hermesAgentSkillRoots(rt, target)
	result := map[string]any{
		"status": "completed", "agent_id": target.ID, "skill_key": target.SkillKey, "supported": supported,
	}
	if err == nil && supported {
		if target.SkillKey == "" {
			result["skills"], _, err = listLocalSkillsInRoots(rt.Provider, roots)
		} else {
			result["preview"], err = readHermesSkillPreview(roots, target.SkillKey)
		}
	}
	if err != nil {
		d.logger.Warn("Hermes agent skills discovery failed", "agent_id", pending.HermesAgent.ID, "error", err)
		d.reportLocalSkillListResult(ctx, rt, pending.ID, map[string]any{
			"status": "failed",
			"error":  "Hermes agent skills discovery failed",
		})
		return
	}
	d.reportLocalSkillListResult(ctx, rt, pending.ID, result)
}

func (d *Daemon) hermesAgentSkillRoots(rt Runtime, target protocol.HermesSkillInventoryAgent) ([]localSkillRoot, bool, error) {
	if rt.Provider != "hermes" {
		return nil, false, nil
	}
	var fixedArgs []string
	if spec, ok := d.customProfileLaunchForRuntime(rt.ID); ok {
		fixedArgs = agent.FilterLaunchPrefix(rt.Provider, spec.fixedArgs, d.logger)
	} else if rt.ProfileID != "" {
		return nil, true, fmt.Errorf("Hermes runtime profile command is unavailable")
	}
	profile := resolveHermesLaunchProfile(target.Home, fixedArgs, target.Args, d.logger)
	if profile.Err != nil {
		return nil, true, profile.Err
	}
	if profile.MustExist {
		if info, err := os.Stat(profile.SourceHome); err != nil || !info.IsDir() {
			return nil, true, fmt.Errorf("Hermes profile home is unavailable")
		}
	}

	profileDirs, err := execenv.HermesProfileSkillDirs(profile.SourceHome, map[string]string{"HERMES_HOME": profile.SourceHome})
	if err != nil {
		return nil, true, err
	}
	roots := make([]localSkillRoot, 0, len(profileDirs)+2)
	if store := execenv.HermesAgentSkillStorePath(d.cfg.Profile, target.ID, profile.SourceHome); store != "" {
		roots = append(roots,
			localSkillRoot{path: store, kind: localSkillRootHermesAgent},
			localSkillRoot{path: execenv.HermesSessionSkillStore(store), kind: localSkillRootHermesAgent},
		)
	}
	for _, dir := range profileDirs {
		roots = append(roots, localSkillRoot{path: dir, kind: localSkillRootHermesProfile})
	}
	runtimeRoots, _, err := localSkillRootsForProvider(rt.Provider)
	if err != nil {
		return nil, true, err
	}
	for _, root := range runtimeRoots {
		if root.kind == localSkillRootUniversal {
			roots = append(roots, root)
		}
	}
	return roots, true, nil
}

// Preview accepts a canonical skill key and confines file reads to the selected
// bundle. A linked skill directory is allowed, as it is in the inventory.
func readHermesSkillPreview(roots []localSkillRoot, key string) (*protocol.HermesSkillPreview, error) {
	canonical, err := normalizeLocalSkillKey(key)
	if err != nil || canonical != key || strings.ContainsAny(key, "\\:") {
		return nil, fmt.Errorf("invalid skill key")
	}
	parts := strings.Split(key, "/")
	if len(parts) > maxLocalSkillDirDepth+1 {
		return nil, fmt.Errorf("invalid skill key")
	}
	for _, part := range parts {
		if isIgnoredLocalSkillEntry(part) {
			return nil, fmt.Errorf("invalid skill key")
		}
	}
	for _, root := range roots {
		dir := filepath.Join(root.path, filepath.FromSlash(key))
		bundle, err := os.OpenRoot(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		preview, err := readHermesSkillBundle(bundle)
		bundle.Close()
		if os.IsNotExist(err) {
			continue
		}
		return preview, err
	}
	return nil, fmt.Errorf("Hermes skill not found")
}

func readHermesSkillBundle(bundle *os.Root) (*protocol.HermesSkillPreview, error) {
	info, err := bundle.Stat("SKILL.md")
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLocalSkillFileSize {
		return nil, fmt.Errorf("SKILL.md is not a supported text file")
	}
	main, err := bundle.Open("SKILL.md")
	if err != nil {
		return nil, err
	}
	defer main.Close()
	content, err := io.ReadAll(io.LimitReader(main, maxLocalSkillFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(content) > int(maxLocalSkillFileSize) || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, fmt.Errorf("SKILL.md is not a supported text file")
	}
	files, err := collectLocalSkillFilesFromFS(bundle.FS(), bundle.Name(), true)
	if err != nil {
		return nil, err
	}
	preview := &protocol.HermesSkillPreview{Content: string(content), Files: make([]protocol.HermesSkillPreviewFile, 0, len(files))}
	for _, file := range files {
		preview.Files = append(preview.Files, protocol.HermesSkillPreviewFile{Path: file.Path, Content: file.Content})
	}
	return preview, nil
}
