package container

import (
	"strings"
	"testing"

	"github.com/bubunyo/monolith/oci"
	"github.com/bubunyo/monolith/vm"
	"github.com/stretchr/testify/require"
)

func container(name, state, imageID string) info {
	return info{name: name, status: &vm.VMStatus{State: state}, cfg: vm.VMConfig{Container: &vm.ContainerConfig{ImageID: imageID}}}
}

func names(cs []info) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

func TestStoppedContainers(t *testing.T) {
	all := []info{
		container("exited", vm.StatusStopped, "a"),
		container("running", vm.StatusRunning, "a"),
		container("paused", vm.StatusPaused, "a"),
		container("suspended", vm.StatusSuspended, "a"),
		container("created", vm.StatusStopped, "b"),
	}
	require.Equal(t, []string{"exited", "created"}, names(stoppedContainers(all)))
}

func TestUnusedImages(t *testing.T) {
	images := []oci.Image{
		{ID: "used", Refs: []string{"index.docker.io/library/nginx:1"}},
		{ID: "tagged", Refs: []string{"index.docker.io/library/alpine:3"}},
		{ID: "dangling"},
		{ID: "dangling-used"},
	}
	containers := []info{
		container("web", vm.StatusStopped, "used"),
		container("old", vm.StatusRunning, "dangling-used"),
	}
	ids := func(imgs []oci.Image) []string {
		var out []string
		for _, i := range imgs {
			out = append(out, i.ID)
		}
		return out
	}
	require.Equal(t, []string{"dangling"}, ids(unusedImages(images, containers, false)))
	require.Equal(t, []string{"tagged", "dangling"}, ids(unusedImages(images, containers, true)))
}

func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, " y \n": true, "n\n": false, "\n": false, "": false, "nope\n": false} {
		require.Equal(t, want, confirm(strings.NewReader(in), "Remove?"), "%q", in)
	}
	require.Equal(t, "container", plural(1, "container"))
	require.Equal(t, "images", plural(2, "image"))
}
