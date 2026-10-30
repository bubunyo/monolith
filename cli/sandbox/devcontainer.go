package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// shellImage picks the shell container's image: the configured one, else
// the repo's devcontainer, else the default. For a devcontainer with a
// Dockerfile it returns a local tag and the `docker build` arguments
// (Dockerfile and context on the Mac; the CLI uploads the context, nothing
// in the repo runs on the Mac).
func shellImage(repo, configured string) (image string, build []string, err error) {
	if configured != "" {
		return configured, nil, nil
	}
	for _, rel := range []string{".devcontainer/devcontainer.json", ".devcontainer.json"} {
		path := filepath.Join(repo, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var dc struct {
			Image string `json:"image"`
			Build struct {
				Dockerfile string `json:"dockerfile"`
				Context    string `json:"context"`
			} `json:"build"`
			Dockerfile string `json:"dockerFile"` // older spelling
		}
		if err := json.Unmarshal(stripJSONC(data), &dc); err != nil {
			return "", nil, fmt.Errorf("parsing %s: %w", rel, err)
		}
		if dc.Image != "" {
			return dc.Image, nil, nil
		}
		dockerfile := dc.Build.Dockerfile
		if dockerfile == "" {
			dockerfile = dc.Dockerfile
		}
		if dockerfile == "" {
			continue
		}
		base := filepath.Dir(path)
		context := dc.Build.Context
		if context == "" {
			context = "."
		}
		dockerfile = filepath.Join(base, dockerfile)
		context = filepath.Join(base, context)
		for _, p := range []string{dockerfile, context} {
			if p != repo && !within(p, repo) {
				return "", nil, fmt.Errorf("%s points outside the repo (%s)", rel, p)
			}
		}
		sum := sha256.Sum256([]byte(dockerfile))
		return "monolith-devcontainer:" + hex.EncodeToString(sum[:4]), []string{"-f", dockerfile, context}, nil
	}
	return defaultImage, nil, nil
}

// stripJSONC removes // and /* */ comments and trailing commas outside
// strings, which devcontainer.json allows.
func stripJSONC(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString, escaped := false, false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
			if i < len(in) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			i += 2
			for i+1 < len(in) && (in[i] != '*' || in[i+1] != '/') {
				i++
			}
			i++
		case c == ',':
			// Drop a comma followed only by whitespace and a closing bracket.
			j := i + 1
			for j < len(in) && (in[j] == ' ' || in[j] == '\t' || in[j] == '\n' || in[j] == '\r') {
				j++
			}
			if j < len(in) && (in[j] == '}' || in[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// within reports whether path is inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && (len(rel) < 3 || rel[:3] != "../")
}
