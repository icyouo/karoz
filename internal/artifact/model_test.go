package artifact

import "testing"

func TestArtifactPolicyContract(t *testing.T) {
	cases := map[string]string{
		"checkout.html": "mockup_html", "flow.svg": "mockup_svg", "design-system.md": "design_system", "requirements.md": "requirements",
	}
	for path, want := range cases {
		if got := InferKind(path); got != want {
			t.Fatalf("InferKind(%q) = %q want %q", path, got, want)
		}
	}
	if !ValidKind("review_report") || ValidKind("binary_blob") {
		t.Fatal("artifact kind policy changed")
	}
	if !ValidStatusTransition("draft", "reviewing") || !ValidStatusTransition("reviewing", "approved") || ValidStatusTransition("approved", "draft") {
		t.Fatal("artifact status transition policy changed")
	}
	if !RequiresApprovalForBuild("mockup_html") || RequiresApprovalForBuild("technical_plan") {
		t.Fatal("artifact build approval policy changed")
	}
}
