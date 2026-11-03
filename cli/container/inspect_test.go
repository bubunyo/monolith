package container

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/bubunyo/monolith/oci"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/require"
)

func TestSplitImagePath(t *testing.T) {
	for in, want := range map[string][2]string{
		"nginx:1.29:/etc/nginx":                {"nginx:1.29", "/etc/nginx"},
		"alpine:/etc/os-release":               {"alpine", "/etc/os-release"},
		"localhost:5000/app:dev:/srv/app.conf": {"localhost:5000/app:dev", "/srv/app.conf"},
		"ghcr.io/o/r@sha256:abc:/":             {"ghcr.io/o/r@sha256:abc", "/"},
	} {
		ref, p, err := splitImagePath(in)
		require.NoError(t, err, in)
		require.Equal(t, want, [2]string{ref, p}, in)
	}
	for _, bad := range []string{"nginx:1.29", "nginx:relative/path", ":/etc"} {
		_, _, err := splitImagePath(bad)
		require.Error(t, err, bad)
	}
}

func TestBuildStep(t *testing.T) {
	require.Equal(t, `CMD ["nginx"]`, buildStep(`/bin/sh -c #(nop)  CMD ["nginx"]`))
	require.Equal(t, "RUN /bin/sh -c apk add curl", buildStep("RUN /bin/sh -c apk add curl # buildkit"))
	require.Equal(t, "ENV A=1", buildStep("ENV A=1"))
}

func TestHistoryRows(t *testing.T) {
	ts := v1.Time{Time: time.Now().Add(-48 * time.Hour)}
	img := oci.Image{
		LayerSizes: []int64{30_000_000, 1_500},
		History: []v1.History{
			{CreatedBy: "# debian.sh", Created: ts},
			{CreatedBy: "ENV A=1", EmptyLayer: true, Created: ts},
			{CreatedBy: "COPY x /x # buildkit", Created: ts},
			{CreatedBy: `/bin/sh -c #(nop)  CMD ["x"]`, EmptyLayer: true},
		},
	}
	rows := historyRows(img)
	require.Equal(t, []historyRow{
		{created: "?", step: `CMD ["x"]`, size: "0B"},
		{created: "2 days ago", step: "COPY x /x", size: "1.5kB"},
		{created: "2 days ago", step: "ENV A=1", size: "0B"},
		{created: "2 days ago", step: "# debian.sh", size: "30.0MB"},
	}, rows, "newest first, sizes on the steps that made layers")
}

func TestLsMode(t *testing.T) {
	require.Equal(t, "-rwsr-xr-x", lsMode(0755|fs.ModeSetuid))
	require.Equal(t, "-rwxr-sr-x", lsMode(0755|fs.ModeSetgid))
	require.Equal(t, "drwxrwxrwt", lsMode(0777|fs.ModeDir|fs.ModeSticky))
	require.Equal(t, "-rwSr--r--", lsMode(0644|fs.ModeSetuid), "setuid without execute")
	require.Equal(t, "lrwxrwxrwx", lsMode(0777|fs.ModeSymlink))
	require.True(t, strings.HasSuffix(lsTime(time.Date(2020, 3, 4, 5, 6, 0, 0, time.Local)), "2020"))
}
