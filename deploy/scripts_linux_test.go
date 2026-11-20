package deploy_test

import (
	"os/exec"
	"testing"
)

func TestDeploymentScripts(t *testing.T) {
	if output, err := exec.Command("bash", "scripts_test.sh").CombinedOutput(); err != nil {
		t.Fatalf("deployment scripts: %v\n%s", err, output)
	}
}
