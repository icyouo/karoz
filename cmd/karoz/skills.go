package main

import (
	"encoding/json"
	skilldomain "github.com/karoz/karoz/internal/skill"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Skill = skilldomain.Skill

var skillMentionPattern = regexp.MustCompile(`(?:^|\s)([$/])([A-Za-z0-9_.:-]+)`)

func (a *app) discoverSkills(project Project) []Skill {
	roots := skillRoots(project)
	seen := map[string]bool{}
	var skills []Skill
	for _, root := range roots {
		base := strings.TrimSpace(root.path)
		if base == "" {
			continue
		}
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != base && strings.HasPrefix(d.Name(), ".") && d.Name() != ".agents" && d.Name() != ".codex" && d.Name() != ".system" {
					return filepath.SkipDir
				}
				if rel, relErr := filepath.Rel(base, path); relErr == nil && rel != "." && len(strings.Split(rel, string(os.PathSeparator))) > 6 {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "SKILL.md" {
				return nil
			}
			skill, ok := readSkillMetadata(path, root.scope)
			if !ok || seen[skill.Name] {
				return nil
			}
			seen[skill.Name] = true
			skills = append(skills, skill)
			return nil
		})
	}
	sort.SliceStable(skills, func(i, j int) bool {
		if skills[i].Scope != skills[j].Scope {
			return skills[i].Scope < skills[j].Scope
		}
		return skills[i].Name < skills[j].Name
	})
	return skills
}

type skillRoot struct {
	path  string
	scope string
}

func skillRoots(project Project) []skillRoot {
	home, _ := os.UserHomeDir()
	codexHome := expandHome(getenv("CODEX_HOME", "~/.codex"))
	var roots []skillRoot
	if strings.TrimSpace(project.Path) != "" {
		roots = append(roots,
			skillRoot{path: filepath.Join(project.Path, ".agents", "skills"), scope: "project"},
			skillRoot{path: filepath.Join(project.Path, ".codex", "skills"), scope: "project"},
		)
	}
	if home != "" {
		roots = append(roots, skillRoot{path: filepath.Join(home, ".agents", "skills"), scope: "user"})
	}
	roots = append(roots,
		skillRoot{path: filepath.Join(codexHome, "skills"), scope: "codex"},
		skillRoot{path: filepath.Join(codexHome, "skills", ".system"), scope: "system"},
	)
	return roots
}

func readSkillMetadata(path, scope string) (Skill, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}
	meta := parseSkillFrontmatter(string(data))
	name := strings.TrimSpace(meta["name"])
	description := strings.TrimSpace(meta["description"])
	if name == "" || description == "" {
		return Skill{}, false
	}
	return Skill{
		Name:             name,
		Description:      description,
		ShortDescription: strings.TrimSpace(firstNonEmpty(meta["short-description"], meta["short_description"])),
		Path:             path,
		Scope:            scope,
	}, true
}

func parseSkillFrontmatter(content string) map[string]string {
	out := map[string]string{}
	content = strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return out
	}
	lines := strings.Split(content, "\n")
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if line == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func (a *app) renderSkillsPrompt(project Project) string {
	skills := a.discoverSkills(project)
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n### Available skills\n")
	b.WriteString("- Skills are local instruction packs. Use list_skills to discover them and read_skill before applying a selected skill.\n")
	b.WriteString("- If the user mentions $SkillName, read that skill first and follow its instructions when relevant.\n")
	for _, skill := range skills {
		b.WriteString("- $")
		b.WriteString(skill.Name)
		b.WriteString(" [")
		b.WriteString(skill.Scope)
		b.WriteString("]: ")
		b.WriteString(limitString(firstNonEmpty(skill.ShortDescription, skill.Description), 260))
		b.WriteString("\n")
	}
	return b.String()
}

func (a *app) injectMentionedSkills(project Project, userText string) string {
	skills := a.discoverSkills(project)
	if len(skills) == 0 {
		return ""
	}
	wanted := map[string]bool{}
	for _, match := range skillMentionPattern.FindAllStringSubmatch(userText, -1) {
		if len(match) > 2 {
			wanted[strings.ToLower(match[2])] = true
		}
	}
	if strings.Contains(strings.ToLower(userText), "skill://") || strings.Contains(userText, "SKILL.md") {
		for _, skill := range skills {
			if strings.Contains(userText, skill.Name) || strings.Contains(userText, skill.Path) {
				wanted[strings.ToLower(skill.Name)] = true
			}
		}
	}
	if len(wanted) == 0 {
		return ""
	}
	var b strings.Builder
	for _, skill := range skills {
		if !wanted[strings.ToLower(skill.Name)] {
			continue
		}
		data, err := os.ReadFile(skill.Path)
		if err != nil {
			continue
		}
		b.WriteString("\n<skill name=\"")
		b.WriteString(skill.Name)
		b.WriteString("\" path=\"")
		b.WriteString(skill.Path)
		b.WriteString("\">\n")
		b.WriteString(limitString(string(data), 60000))
		b.WriteString("\n</skill>\n")
	}
	return b.String()
}

func (a *app) listSkillsTool(project Project, query string) string {
	query = strings.ToLower(strings.TrimSpace(query))
	var out []Skill
	for _, skill := range a.discoverSkills(project) {
		haystack := strings.ToLower(skill.Name + "\n" + skill.Description + "\n" + skill.ShortDescription + "\n" + skill.Scope)
		if query == "" || strings.Contains(haystack, query) {
			out = append(out, skill)
		}
	}
	return toolJSON(map[string]any{"skills": out})
}

func (a *app) readSkillTool(project Project, name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "name is required"})
	}
	for _, skill := range a.discoverSkills(project) {
		if strings.ToLower(skill.Name) != name {
			continue
		}
		data, err := os.ReadFile(skill.Path)
		if err != nil {
			return toolJSON(map[string]any{"error": "read_failed", "message": err.Error()})
		}
		return toolJSON(map[string]any{"skill": skill, "content": string(data)})
	}
	return toolJSON(map[string]any{"error": "not_found", "message": "skill not found"})
}

func skillsJSON(skills []Skill) string {
	data, err := json.Marshal(skills)
	if err != nil {
		return "[]"
	}
	return string(data)
}
