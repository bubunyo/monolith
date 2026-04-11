package monolith

import "testing"

func TestRun(t *testing.T) {
	err := Run(".")
	if err != nil {
		t.Log(err)
	}
}
