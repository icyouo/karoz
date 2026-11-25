package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/karoz/karoz/internal/skillfetch"
)

func makeSkillTarGz(t *testing.T, name, desc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\n"
	hdr := &tar.Header{Name: "acme-repo-deadbeef/SKILL.md", Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

func TestSkillsAPIListDiscoverAndDelete(t *testing.T) {
	a, project := newHandlerTestApp(t)
	handler := a.httpHandler()

	// Stage an installed skill directly via skillfetch into a.installedSkillsDir()
	archive := makeSkillTarGz(t, "release-check", "Verify release notes and checksums")
	if _, err := skillfetch.InstallFromTarGz(bytes.NewReader(archive), a.installedSkillsDir(), skillfetch.RepoSpec{Owner: "acme", Repo: "release-check"}); err != nil {
		t.Fatalf("InstallFromTarGz failed: %v", err)
	}

	// Verify GET /api/skills lists it
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/skills", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "release-check") {
		t.Fatalf("GET /api/skills failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Verify project skill discovery includes scope="installed"
	discovered := a.discoverSkills(project)
	found := false
	for _, s := range discovered {
		if s.Name == "release-check" && s.Scope == "installed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected installed skill in discoverSkills, got %+v", discovered)
	}

	// Reject invalid source via POST /api/skills
	badReq := httptest.NewRequest(http.MethodPost, "/api/skills", strings.NewReader(`{"source":"https://gitlab.com/evil/repo"}`))
	badReq.Header.Set("Content-Type", "application/json")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad GitHub source, got %d body=%s", badRec.Code, badRec.Body.String())
	}

	// Delete installed skill via DELETE /api/skills/release-check
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, httptest.NewRequest(http.MethodDelete, "/api/skills/release-check", nil))
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/skills/release-check status=%d body=%s", delRec.Code, delRec.Body.String())
	}
	var delBody map[string]any
	_ = json.Unmarshal(delRec.Body.Bytes(), &delBody)
	if delBody["removed"] != true {
		t.Fatalf("unexpected delete response: %s", delRec.Body.String())
	}
}
