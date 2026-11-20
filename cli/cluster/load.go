package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"

	"github.com/bubunyo/monolith/internal/ansi"
	"github.com/bubunyo/monolith/oci"
	"github.com/bubunyo/monolith/vm"
)

const defaultDockerHost = "docker-lith-default"

var (
	flagLoadFile        string
	flagLoadInputs      []string
	flagLoadPlatform    string
	flagLoadFrom        string
	flagLoadConcurrency int
)

var loadCmd = &cobra.Command{
	Use:   "load [IMAGE...]",
	Short: "Load images into a cluster's node without a registry",
	Long: `Load images into the cluster's containerd, so pods that use the same name
run them without pulling (set imagePullPolicy: IfNotPresent or Never for
:latest tags, which Kubernetes would otherwise pull again).

Images come from their registries by default. List them as arguments, in a
file (-f FILE, one per line; blank lines and # comments are skipped), or on
stdin:

  monolith cluster load nginx:1.27 redis:7
  cat images.txt | monolith cluster load

The arm64 variant is loaded when there is one. An image that only exists for
linux/amd64 is relabelled as arm64 and runs through Rosetta, which the
cluster must have ('cluster start --rosetta'). --platform picks a variant.

--from docker[:NAME] takes the images from a monolith docker host instead
(default docker-lith-default), e.g. ones you built there. -i FILE loads a
'docker save' archive as it is ('-' reads it from stdin).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return load(cmd, args)
	},
}

// loadJob is one image (or archive) to load.
type loadJob struct {
	ref     string   // as given; also the name it's loaded under
	tag     name.Tag // registry and docker sources
	archive string   // -i: a file, or "-" for stdin
}

func load(cmd *cobra.Command, args []string) error {
	if err := checkCluster(flagName); err != nil {
		return err
	}
	cfg, err := vm.ReadConfig(flagName)
	if err != nil {
		return err
	}
	if st, err := vm.ReadState(flagName); err != nil || st.State != vm.StatusRunning {
		state := vm.StatusStopped
		if st != nil {
			state = st.State
		}
		return fmt.Errorf("cluster %q is %s; start it with 'monolith cluster start --name %s'", flagName, state, flagName)
	}
	if cfg.GuestLayout < vm.GuestLayoutImageLoad {
		return fmt.Errorf("cluster %q was created before 'cluster load'; recreate it ('monolith cluster rm --name %s', then start) to use it", flagName, flagName)
	}
	if flagLoadConcurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1")
	}
	platform, err := oci.ParseLoadPlatform(flagLoadPlatform)
	if err != nil {
		return err
	}
	dockerHost, err := parseFrom(flagLoadFrom)
	if err != nil {
		return err
	}
	if dockerHost != "" && platform != nil {
		return fmt.Errorf("--platform applies to registry images; a docker host has one variant of each image")
	}

	jobs, err := loadJobs(cmd, args)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return fmt.Errorf("no images given: pass them as arguments, with -f FILE, on stdin, or -i ARCHIVE")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var store *oci.Store
	if dockerHost == "" {
		if store, err = oci.Open(); err != nil {
			return err
		}
		unlock, err := store.SharedLock()
		if err != nil {
			return err
		}
		defer unlock()
	}
	var docker *http.Client
	if dockerHost != "" {
		if docker, err = dockerClient(dockerHost); err != nil {
			return err
		}
	}

	l := &loader{
		cluster: flagName, rosetta: cfg.Rosetta, platform: platform,
		store: store, dockerHost: dockerHost, docker: docker,
	}
	tty := ansi.IsTerminal(os.Stdout)
	view := oci.NewLoadView(os.Stdout, tty)
	groups := make([]*oci.LoadGroup, len(jobs))
	for i, j := range jobs {
		groups[i] = view.Add(j.ref)
	}
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, flagLoadConcurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				groups[i].Fail(ctx.Err())
				return
			}
			defer func() { <-sem }()
			errs[i] = l.run(ctx, j, groups[i])
			if errs[i] != nil {
				groups[i].Fail(errs[i])
			} else {
				groups[i].Done()
			}
		}()
	}
	wg.Wait()
	view.Close()
	return summarize(os.Stdout, tty, flagName, jobs, errs, l.notes())
}

// parseFrom turns --from into a docker host name ("" for registries).
func parseFrom(from string) (string, error) {
	switch {
	case from == "" || from == "registry":
		return "", nil
	case from == "docker":
		return defaultDockerHost, nil
	case strings.HasPrefix(from, "docker:") && len(from) > len("docker:"):
		return strings.TrimPrefix(from, "docker:"), nil
	}
	return "", fmt.Errorf("--from must be registry, docker or docker:NAME, not %q", from)
}

// loadJobs collects the images from the arguments, -f, stdin and -i,
// without duplicates.
func loadJobs(cmd *cobra.Command, args []string) ([]loadJob, error) {
	refs := append([]string{}, args...)
	stdinUsed := false
	for _, in := range flagLoadInputs {
		if in == "-" {
			stdinUsed = true
		}
	}
	switch {
	case flagLoadFile == "-":
		if stdinUsed {
			return nil, fmt.Errorf("stdin can't hold both the image list (-f -) and an archive (-i -)")
		}
		list, err := readImageList(os.Stdin)
		if err != nil {
			return nil, err
		}
		refs = append(refs, list...)
	case flagLoadFile != "":
		f, err := os.Open(flagLoadFile)
		if err != nil {
			return nil, err
		}
		list, err := readImageList(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		refs = append(refs, list...)
	case len(args) == 0 && len(flagLoadInputs) == 0 && !ansi.IsTerminal(os.Stdin):
		list, err := readImageList(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		refs = append(refs, list...)
	}

	var jobs []loadJob
	seen := map[string]bool{}
	for _, ref := range refs {
		tag, err := oci.LoadRef(ref)
		if err != nil {
			return nil, err
		}
		if seen[tag.Name()] {
			continue
		}
		seen[tag.Name()] = true
		jobs = append(jobs, loadJob{ref: ref, tag: tag})
	}
	for _, in := range flagLoadInputs {
		jobs = append(jobs, loadJob{ref: in, archive: in})
	}
	return jobs, nil
}

// readImageList reads one image per line; blank lines and # comments are
// skipped.
func readImageList(r io.Reader) ([]string, error) {
	var refs []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, " \t") {
			return nil, fmt.Errorf("line %d: one image per line, got %q", n, line)
		}
		refs = append(refs, line)
	}
	return refs, sc.Err()
}

// loader loads one job at a time into a cluster.
type loader struct {
	cluster    string
	rosetta    bool
	platform   *v1.Platform
	store      *oci.Store
	dockerHost string
	docker     *http.Client

	mu     sync.Mutex
	latest []string // :latest images not in a registry as loaded
}

func (l *loader) run(ctx context.Context, j loadJob, g *oci.LoadGroup) error {
	switch {
	case j.archive != "":
		return l.loadArchive(ctx, j.archive, g)
	case l.dockerHost != "":
		return l.loadFromDocker(ctx, j, g)
	}
	return l.loadFromRegistry(ctx, j, g)
}

func (l *loader) noRosetta() error {
	return fmt.Errorf("only linux/amd64 is available, which needs Rosetta: stop the cluster and run 'monolith cluster start --name %s --rosetta'", l.cluster)
}

func (l *loader) noteLatest(j loadJob) {
	if j.tag.TagStr() == "latest" {
		l.mu.Lock()
		l.latest = append(l.latest, j.ref)
		l.mu.Unlock()
	}
}

func (l *loader) notes() []string {
	var notes []string
	for _, ref := range l.latest {
		notes = append(notes, fmt.Sprintf("%s is a :latest tag; pods must use imagePullPolicy: IfNotPresent or Never, or Kubernetes pulls it again", ref))
	}
	return notes
}

func (l *loader) loadFromRegistry(ctx context.Context, j loadJob, g *oci.LoadGroup) error {
	g.Stage("Resolving", 0, 0)
	r, err := oci.Resolve(ctx, j.tag, l.platform)
	if err != nil {
		return err
	}
	if r.Relabel && !l.rosetta {
		return l.noRosetta()
	}
	note := "arm64"
	if r.Relabel {
		note = "amd64, relabelled for Rosetta"
		l.noteLatest(j)
	}
	ids := make([]string, len(r.Layers))
	for i, layer := range r.Layers {
		d, err := layer.Digest()
		if err != nil {
			return err
		}
		ids[i] = d.Hex[:12]
	}
	g.Start(note, ids)
	g.Stage(fmt.Sprintf("Downloading %d layer%s (%s)", len(ids), plural(len(ids)), oci.HumanSize(r.Size)), 0, 0)
	if err := l.store.Fetch(ctx, r, g); err != nil {
		return err
	}
	stage := "Loading into cluster-" + l.cluster
	g.Stage(stage, 0, r.Size)
	return l.stream(ctx, func(w io.Writer) error {
		return l.store.WriteTarball(r, j.tag, w, func(n int64) { g.Stage(stage, n, r.Size) })
	})
}

// stream pipes what write produces into the cluster, stopping on Ctrl+C.
func (l *loader) stream(ctx context.Context, write func(io.Writer) error) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(write(pw)) }()
	stop := context.AfterFunc(ctx, func() { pr.CloseWithError(ctx.Err()) })
	defer stop()
	err := vm.LoadImage(l.cluster, pr)
	pr.CloseWithError(fmt.Errorf("load finished"))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (l *loader) loadArchive(ctx context.Context, path string, g *oci.LoadGroup) error {
	var r io.Reader = os.Stdin
	var size int64
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			size = fi.Size()
		}
		r = f
	}
	g.Start("archive", nil)
	stage := "Loading into cluster-" + l.cluster
	g.Stage(stage, 0, size)
	return l.stream(ctx, func(w io.Writer) error {
		_, err := io.Copy(w, &countReader{r: r, fn: func(n int64) { g.Stage(stage, n, size) }})
		return err
	})
}

func (l *loader) loadFromDocker(ctx context.Context, j loadJob, g *oci.LoadGroup) error {
	g.Stage("Resolving", 0, 0)
	info, err := dockerInspect(ctx, l.docker, j.ref)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", j.ref, l.dockerHost, err)
	}
	l.noteLatest(j)
	switch info.Architecture {
	case "arm64":
		g.Start("arm64, from "+l.dockerHost, nil)
		stage := "Loading from " + l.dockerHost + " into cluster-" + l.cluster
		g.Stage(stage, 0, info.Size)
		return l.stream(ctx, func(w io.Writer) error {
			body, err := dockerSave(ctx, l.docker, j.ref)
			if err != nil {
				return err
			}
			defer body.Close()
			_, err = io.Copy(w, &countReader{r: body, fn: func(n int64) { g.Stage(stage, n, info.Size) }})
			return err
		})
	case "amd64":
		if !l.rosetta {
			return l.noRosetta()
		}
	default:
		return fmt.Errorf("%s is linux/%s; a cluster runs arm64, or amd64 with Rosetta", j.ref, info.Architecture)
	}

	// Relabelling reads the archive twice, so it's saved to a file first.
	g.Start("amd64 from "+l.dockerHost+", relabelled for Rosetta", nil)
	dir, err := os.MkdirTemp("", "monolith-load-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "image.tar")
	stage := "Exporting from " + l.dockerHost
	g.Stage(stage, 0, info.Size)
	if err := saveToFile(ctx, l.docker, j.ref, path, func(n int64) { g.Stage(stage, n, info.Size) }); err != nil {
		return err
	}
	stage = "Loading into cluster-" + l.cluster
	g.Stage(stage, 0, info.Size)
	return l.stream(ctx, func(w io.Writer) error {
		return oci.RelabelArchive(path, j.tag, w, func(n int64) { g.Stage(stage, n, info.Size) })
	})
}

// dockerClient talks to a monolith docker host's engine API.
func dockerClient(host string) (*http.Client, error) {
	if !vm.Exists(host) {
		return nil, fmt.Errorf("docker host %q does not exist", host)
	}
	cfg, err := vm.ReadConfig(host)
	if err != nil {
		return nil, err
	}
	if cfg.Image != vm.ImageDocker || vm.EngineOf(cfg) != vm.EngineDocker || cfg.Sandbox != nil {
		return nil, fmt.Errorf("%q isn't a docker engine host; --from docker needs one", host)
	}
	if st, err := vm.ReadState(host); err != nil || st.State != vm.StatusRunning {
		return nil, fmt.Errorf("docker host %q isn't running; start it with 'monolith docker start --name %s'", host, host)
	}
	sock, err := vm.VMDockerSockPath(host)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}, nil
}

type dockerImage struct {
	Architecture string `json:"Architecture"`
	Size         int64  `json:"Size"`
}

func dockerInspect(ctx context.Context, c *http.Client, ref string) (dockerImage, error) {
	var info dockerImage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/images/"+ref+"/json", nil)
	if err != nil {
		return info, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return info, fmt.Errorf("no such image")
	}
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("docker engine: %s", resp.Status)
	}
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

// dockerSave streams `docker save ref` from the engine.
func dockerSave(ctx context.Context, c *http.Client, ref string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/images/get?names="+ref, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("docker engine: exporting %s: %s", ref, resp.Status)
	}
	return resp.Body, nil
}

func saveToFile(ctx context.Context, c *http.Client, ref, path string, progress func(int64)) error {
	body, err := dockerSave(ctx, c, ref)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, &countReader{r: body, fn: progress})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// countReader reports the bytes read so far through fn.
type countReader struct {
	r  io.Reader
	n  int64
	fn func(int64)
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.fn(c.n)
	return n, err
}

// summarize prints how many images loaded and why the rest didn't; the
// error makes the exit status non-zero when any failed.
func summarize(w io.Writer, tty bool, cluster string, jobs []loadJob, errs []error, notes []string) error {
	color := ansi.UseColor(tty)
	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	fmt.Fprintln(w)
	for _, n := range notes {
		fmt.Fprintln(w, ansi.Paint(color, "33", "Note: "+n))
	}
	loaded := len(jobs) - failed
	fmt.Fprintf(w, "Loaded %d of %d image%s into cluster-%s.\n", loaded, len(jobs), plural(len(jobs)), cluster)
	if failed == 0 {
		return nil
	}
	for i, err := range errs {
		if err != nil {
			fmt.Fprintf(w, "  %s %s: %v\n", ansi.Paint(color, "31", "✘"), jobs[i].ref, err)
		}
	}
	return fmt.Errorf("%d image%s failed to load", failed, plural(failed))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
