package skillfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type tarFixtureEntry struct {
	name     string
	content  string
	typeflag byte
	linkname string
}

func buildTarGz(t *testing.T, entries []tarFixtureEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, entry := range entries {
		tf := entry.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     0644,
			Size:     int64(len(entry.content)),
			Typeflag: tf,
			Linkname: entry.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg && len(entry.content) > 0 {
			if _, err := tw.Write([]byte(entry.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseGitHubSource(t *testing.T) {
	spec, err := ParseGitHubSource("acme/my-skill")
	if err != nil || spec.Owner != "acme" || spec.Repo != "my-skill" || spec.Ref != "" {
		t.Fatalf("unexpected shorthand parse: %+v err=%v", spec, err)
	}

	spec, err = ParseGitHubSource("https://github.com/acme/skills/tree/main/tools/deploy")
	if err != nil || spec.Owner != "acme" || spec.Repo != "skills" || spec.Ref != "main" || spec.Subpath != "tools/deploy" {
		t.Fatalf("unexpected tree URL parse: %+v err=%v", spec, err)
	}

	for _, bad := range []string{"", "https://gitlab.com/acme/repo", "http://github.com/acme/repo", "../etc/passwd", "acme/repo/tree/main/../../bad"} {
		if _, err := ParseGitHubSource(bad); err == nil {
			t.Fatalf("expected invalid source error for %q", bad)
		}
	}
}

func TestInstallFromURLAndSecurityChecks(t *testing.T) {
	validArchive := buildTarGz(t, []tarFixtureEntry{
		{name: "acme-skills-abc/tools/my-skill/SKILL.md", content: "---\nname: my-skill\ndescription: Helps deploy apps\n---\nInstructions here.\n"},
		{name: "acme-skills-abc/tools/my-skill/README.md", content: "Extra docs"},
	})
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: int64(len(validArchive)),
				Body:          io.NopCloser(bytes.NewReader(validArchive)),
				Header:        make(http.Header),
				Request:       r,
			}, nil
		}),
	}

	dest := filepath.Join(t.TempDir(), "skills")
	spec, err := ParseGitHubSource("https://github.com/acme/skills/tree/main/tools/my-skill")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := InstallFromURL(context.Background(), client, dest, spec, spec.TarballURL())
	if err != nil {
		t.Fatalf("InstallFromURL failed: %v", err)
	}
	if installed.Slug != "my-skill" || installed.Name != "my-skill" {
		t.Fatalf("unexpected installed skill: %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(dest, "my-skill", "SKILL.md")); err != nil {
		t.Fatalf("expected extracted SKILL.md: %v", err)
	}

	list, err := ListInstalled(dest)
	if err != nil || len(list) != 1 || list[0].Slug != "my-skill" {
		t.Fatalf("unexpected ListInstalled: %+v err=%v", list, err)
	}

	if err := RemoveInstalled(dest, "my-skill"); err != nil {
		t.Fatalf("RemoveInstalled failed: %v", err)
	}
	listAfter, _ := ListInstalled(dest)
	if len(listAfter) != 0 {
		t.Fatalf("expected empty installed list, got %+v", listAfter)
	}

	// Reject path traversal archive
	badTraversal := buildTarGz(t, []tarFixtureEntry{
		{name: "acme-skills-abc/../../evil", content: "pwned"},
	})
	if _, err := InstallFromTarGz(bytes.NewReader(badTraversal), dest, RepoSpec{Owner: "acme", Repo: "skills"}); err == nil {
		t.Fatal("expected traversal archive to fail")
	}

	// Reject symlink archive
	badSymlink := buildTarGz(t, []tarFixtureEntry{
		{name: "acme-skills-abc/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	if _, err := InstallFromTarGz(bytes.NewReader(badSymlink), dest, RepoSpec{Owner: "acme", Repo: "skills"}); err == nil {
		t.Fatal("expected symlink archive to fail")
	}

	// Reject archive missing SKILL.md
	missingSkill := buildTarGz(t, []tarFixtureEntry{
		{name: "acme-skills-abc/README.md", content: "nothing"},
	})
	if _, err := InstallFromTarGz(bytes.NewReader(missingSkill), dest, RepoSpec{Owner: "acme", Repo: "skills"}); err == nil {
		t.Fatal("expected missing SKILL.md archive to fail")
	}
}
