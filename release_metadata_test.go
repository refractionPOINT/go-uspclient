package uspclient

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Retraction metadata is distributed through the next release's go.mod. Verify
// the Go tool recognizes the affected version and its user-visible reason.
func TestAdmissionStallReleaseRetracted(t *testing.T) {
	raw, err := exec.Command("go", "mod", "edit", "-json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Retract []struct{ Low, High, Rationale string }
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	for _, r := range metadata.Retract {
		if r.Low == "v1.6.18" && r.High == "v1.6.18" && strings.Contains(r.Rationale, "admission stall") {
			return
		}
	}
	t.Fatal("v1.6.18 must be retracted with an admission stall reason")
}
