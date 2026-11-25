package skillfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	skilldomain "github.com/karoz/karoz/internal/skill"
)

const (
	MaxExtractedFiles = 256
	MaxExtractedBytes = 32 << 20
	MaxSkillMDBytes   = 512 << 10
)

var (
	validSkillSlugRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	ErrUnsafeArchive = errors.New("unsafe archive entry")
)

type InstalledSkill struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Path        string    `json:"path"`
	InstalledAt time.Time `json:"installed_at"`
}

type extractedFile struct {
	rel  string
	data []byte
	mode os.FileMode
}

// InstallFromTarGz validates and installs a SKILL.md bundle from a gzip-compressed tar stream.
func InstallFromTarGz(r io.Reader, destRoot string, spec RepoSpec) (InstalledSkill, error) {
	if strings.TrimSpace(destRoot) == "" {
		return InstalledSkill{}, errors.New("destination skills directory is required")
	}

	gz, err := gzip.NewReader(io.LimitReader(r, MaxDownloadBytes+1))
	if err != nil {
		return InstalledSkill{}, fmt.Errorf("open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	files := make(map[string]extractedFile)
	var totalBytes int64
	topPrefix := ""

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return InstalledSkill{}, fmt.Errorf("read tar entry: %w", err)
		}

		if hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeReg {
			return InstalledSkill{}, fmt.Errorf("%w: unsupported entry type %q for %q", ErrUnsafeArchive, hdr.Typeflag, hdr.Name)
		}

		cleanName, err := sanitizeTarPath(hdr.Name)
		if err != nil {
			return InstalledSkill{}, err
		}
		if cleanName == "" || cleanName == "." {
			continue
		}

		if topPrefix == "" {
			if idx := strings.IndexByte(cleanName, '/'); idx >= 0 {
				topPrefix = cleanName[:idx]
			} else if hdr.Typeflag == tar.TypeDir {
				topPrefix = cleanName
				continue
			}
		}

		rel := cleanName
		if topPrefix != "" {
			if cleanName == topPrefix {
				continue
			}
			prefixSlash := topPrefix + "/"
			if strings.HasPrefix(cleanName, prefixSlash) {
				rel = strings.TrimPrefix(cleanName, prefixSlash)
			}
		}
		if rel == "" || hdr.Typeflag == tar.TypeDir {
			continue
		}

		if len(files) >= MaxExtractedFiles {
			return InstalledSkill{}, fmt.Errorf("archive exceeds %d file limit", MaxExtractedFiles)
		}
		if hdr.Size < 0 || totalBytes+hdr.Size > MaxExtractedBytes {
			return InstalledSkill{}, fmt.Errorf("extracted size exceeds %d bytes", MaxExtractedBytes)
		}

		data, err := io.ReadAll(io.LimitReader(tr, MaxExtractedBytes-totalBytes+1))
		if err != nil {
			return InstalledSkill{}, fmt.Errorf("read entry %q: %w", rel, err)
		}
		totalBytes += int64(len(data))
		if totalBytes > MaxExtractedBytes {
			return InstalledSkill{}, fmt.Errorf("extracted size exceeds %d bytes", MaxExtractedBytes)
		}

		mode := os.FileMode(0644)
		if hdr.Mode&0111 != 0 {
			mode = 0755
		}
		files[rel] = extractedFile{rel: rel, data: data, mode: mode}
	}

	skillDir, skillName, skillDesc, err := selectSkillRoot(files, spec.Subpath)
	if err != nil {
		return InstalledSkill{}, err
	}

	slug := sanitizeSlug(skillName)
	if slug == "" {
		slug = sanitizeSlug(path.Base(skillDir))
	}
	if !validSkillSlugRE.MatchString(slug) || slug == "." || slug == ".." {
		return InstalledSkill{}, fmt.Errorf("invalid installed skill slug %q", slug)
	}

	if err := os.MkdirAll(destRoot, 0755); err != nil {
		return InstalledSkill{}, err
	}

	stageDir, err := os.MkdirTemp(destRoot, ".skillfetch-*")
	if err != nil {
		return InstalledSkill{}, err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	prefix := ""
	if skillDir != "" {
		prefix = skillDir + "/"
	}
	for rel, f := range files {
		if prefix != "" && !strings.HasPrefix(rel, prefix) {
			continue
		}
		sub := rel
		if prefix != "" {
			sub = strings.TrimPrefix(rel, prefix)
		}
		if sub == "" || strings.HasPrefix(sub, ".") && path.Base(sub) != ".agents" {
			continue
		}
		target := filepath.Join(stageDir, filepath.FromSlash(sub))
		if !strings.HasPrefix(target, stageDir+string(filepath.Separator)) {
			return InstalledSkill{}, fmt.Errorf("%w: escapes target dir", ErrUnsafeArchive)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return InstalledSkill{}, err
		}
		if err := os.WriteFile(target, f.data, f.mode); err != nil {
			return InstalledSkill{}, err
		}
	}

	finalDir := filepath.Join(destRoot, slug)
	backupDir := finalDir + ".bak-" + fmt.Sprintf("%d", time.Now().UnixNano())
	hadExisting := false
	if _, err := os.Stat(finalDir); err == nil {
		hadExisting = true
		if err := os.Rename(finalDir, backupDir); err != nil {
			return InstalledSkill{}, err
		}
	}
	if err := os.Rename(stageDir, finalDir); err != nil {
		if hadExisting {
			_ = os.Rename(backupDir, finalDir)
		}
		return InstalledSkill{}, err
	}
	if hadExisting {
		_ = os.RemoveAll(backupDir)
	}

	return InstalledSkill{
		Slug:        slug,
		Name:        skillName,
		Description: skillDesc,
		Path:        filepath.Join(finalDir, "SKILL.md"),
		InstalledAt: time.Now().UTC(),
	}, nil
}

func selectSkillRoot(files map[string]extractedFile, subpath string) (dir, name, desc string, err error) {
	sub := strings.Trim(path.Clean(strings.ReplaceAll(subpath, "\\", "/")), "/")
	if sub == "." {
		sub = ""
	}

	type candidate struct {
		dir  string
		name string
		desc string
	}
	var matches []candidate
	for rel, f := range files {
		if path.Base(rel) != "SKILL.md" {
			continue
		}
		entryDir := path.Dir(rel)
		if entryDir == "." {
			entryDir = ""
		}
		if sub != "" && entryDir != sub {
			continue
		}
		if len(f.data) > MaxSkillMDBytes || !utf8.Valid(f.data) {
			return "", "", "", fmt.Errorf("SKILL.md in %q exceeds limit or is not valid UTF-8", entryDir)
		}
		meta := skilldomain.ParseFrontmatter(string(f.data))
		n := strings.TrimSpace(meta["name"])
		d := strings.TrimSpace(meta["description"])
		if n == "" || d == "" {
			continue
		}
		matches = append(matches, candidate{dir: entryDir, name: n, desc: d})
	}
	if len(matches) == 0 {
		if sub != "" {
			return "", "", "", fmt.Errorf("no valid SKILL.md with name/description found at subpath %q", sub)
		}
		return "", "", "", errors.New("no valid SKILL.md with name/description found in repository")
	}
	for _, m := range matches {
		if m.dir == sub {
			return m.dir, m.name, m.desc, nil
		}
	}
	if len(matches) == 1 {
		return matches[0].dir, matches[0].name, matches[0].desc, nil
	}
	return "", "", "", fmt.Errorf("repository contains %d skills; specify the skill subpath with /tree/<ref>/<subpath>", len(matches))
}

func sanitizeTarPath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: NUL byte in path", ErrUnsafeArchive)
	}
	slash := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(slash, "/") || strings.Contains(slash, ":") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafeArchive, raw)
	}
	cleaned := path.Clean(slash)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: traversal %q", ErrUnsafeArchive, raw)
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: traversal %q", ErrUnsafeArchive, raw)
		}
	}
	return strings.TrimSuffix(cleaned, "/"), nil
}

func sanitizeSlug(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

func ListInstalled(destRoot string) ([]InstalledSkill, error) {
	entries, err := os.ReadDir(destRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []InstalledSkill{}, nil
		}
		return nil, err
	}
	var out []InstalledSkill
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !validSkillSlugRE.MatchString(entry.Name()) {
			continue
		}
		skillMD := filepath.Join(destRoot, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(skillMD)
		if err != nil {
			continue
		}
		meta := skilldomain.ParseFrontmatter(string(data))
		info, _ := entry.Info()
		modTime := time.Time{}
		if info != nil {
			modTime = info.ModTime().UTC()
		}
		out = append(out, InstalledSkill{
			Slug:        entry.Name(),
			Name:        strings.TrimSpace(firstNonEmpty(meta["name"], entry.Name())),
			Description: strings.TrimSpace(meta["description"]),
			Path:        skillMD,
			InstalledAt: modTime,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func RemoveInstalled(destRoot, slug string) error {
	clean := strings.TrimSpace(slug)
	if !validSkillSlugRE.MatchString(clean) || clean == "." || clean == ".." {
		return fmt.Errorf("invalid skill slug %q", slug)
	}
	target := filepath.Join(destRoot, clean)
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a skill directory: %q", clean)
	}
	return os.RemoveAll(target)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// Unused import safety guard in case buffer helpers expand later.
var _ = bytes.Compare
