package docker

import "testing"

func TestHostOfContext(t *testing.T) {
	for in, want := range map[string]string{
		"docker-lith-default": "lith-default",
		"docker-a":            "a",
		"default":             "default",
	} {
		if got := hostOfContext(in); got != want {
			t.Errorf("hostOfContext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUseRejectsBadName(t *testing.T) {
	if err := use("Bad_Name"); err == nil {
		t.Fatal("expected an error for an invalid name")
	}
}

func TestUseRejectsMissingHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := use("no-such-host"); err == nil {
		t.Fatal("expected an error for a missing host")
	}
}
