package monolith

import "testing"

func TestRun(t *testing.T) {
	err := Run("test-vm")
	if err != nil {
		t.Log(err)
	}
}
