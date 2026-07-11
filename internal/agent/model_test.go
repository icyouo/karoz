package agent

import "testing"

func TestAgentCapabilityContract(t *testing.T) {
	designer := Agent{ID: "designer", Name: "product-designer", Role: "design interfaces"}
	designerCapabilities := CapabilitiesFor(designer)
	if !designerCapabilities.CanDirectChat || !designerCapabilities.CanCreateArtifacts || !designerCapabilities.CanDesignArtifacts || designerCapabilities.CanManageAgents {
		t.Fatalf("designer capabilities = %+v", designerCapabilities)
	}
	karozCapabilities := CapabilitiesFor(Agent{ID: "karoz", Name: "Karoz"})
	if !karozCapabilities.CanManageAgents || !karozCapabilities.CanManageRoutes || !karozCapabilities.CanReconcileBacklog {
		t.Fatalf("karoz capabilities = %+v", karozCapabilities)
	}
	if !IsReviewer(Agent{Name: "design-critic"}) || !IsBuilder(Agent{Name: "implementation-lead"}) {
		t.Fatal("role classification contract changed")
	}
}
