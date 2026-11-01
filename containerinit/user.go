package containerinit

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Cred is who the process runs as.
type Cred struct {
	UID, GID uint32
	Groups   []uint32 // supplementary groups from /etc/group
	Home     string
}

// ResolveUser resolves a Docker-style user ("", "name", "uid",
// "name:group", "uid:gid") against the passwd and group files of the
// filesystem at root, as runc does: a numeric user that isn't in passwd is
// still allowed, with gid 0 and home "/".
func ResolveUser(root, user string) (Cred, error) {
	userPart, groupPart, hasGroup := strings.Cut(user, ":")
	if userPart == "" {
		userPart = "0"
	}
	passwd, err := readColonFile(filepath.Join(root, "etc/passwd"), 7)
	if err != nil {
		return Cred{}, err
	}
	groups, err := readColonFile(filepath.Join(root, "etc/group"), 4)
	if err != nil {
		return Cred{}, err
	}

	cred := Cred{Home: "/"}
	var name string
	found := false
	for _, f := range passwd {
		if f[0] == userPart || f[2] == userPart {
			uid, err1 := strconv.ParseUint(f[2], 10, 32)
			gid, err2 := strconv.ParseUint(f[3], 10, 32)
			if err1 != nil || err2 != nil {
				continue
			}
			cred.UID, cred.GID, cred.Home, name = uint32(uid), uint32(gid), f[5], f[0]
			found = true
			break
		}
	}
	if !found {
		uid, err := strconv.ParseUint(userPart, 10, 32)
		if err != nil {
			return Cred{}, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
		cred.UID, cred.GID = uint32(uid), 0
		if uid == 0 {
			cred.Home = "/root"
		}
	}

	if hasGroup {
		gid, ok := lookupGroup(groups, groupPart)
		if !ok {
			return Cred{}, fmt.Errorf("unable to find group %s: no matching entries in group file", groupPart)
		}
		cred.GID = gid
	}

	// Supplementary groups: those listing the user as a member.
	if name != "" {
		for _, f := range groups {
			for _, m := range strings.Split(f[3], ",") {
				if m == name {
					if gid, err := strconv.ParseUint(f[2], 10, 32); err == nil {
						cred.Groups = append(cred.Groups, uint32(gid))
					}
				}
			}
		}
	}
	return cred, nil
}

func lookupGroup(groups [][]string, g string) (uint32, bool) {
	for _, f := range groups {
		if f[0] == g || f[2] == g {
			if gid, err := strconv.ParseUint(f[2], 10, 32); err == nil {
				return uint32(gid), true
			}
		}
	}
	if gid, err := strconv.ParseUint(g, 10, 32); err == nil {
		return uint32(gid), true
	}
	return 0, false
}

// readColonFile parses passwd/group style files; a missing file is empty.
func readColonFile(path string, fields int) ([][]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < fields {
			parts = append(parts, make([]string, fields-len(parts))...)
		}
		out = append(out, parts)
	}
	return out, sc.Err()
}

// LookPath finds file the way execvp does, in the filesystem at root: a
// name with a slash is used as is (relative to dir), otherwise each PATH
// entry is tried. It returns the path inside root and the exit code to
// report if it fails (127 not found, 126 not executable).
func LookPath(root, dir, file, pathEnv string) (string, int, error) {
	check := func(p string) (bool, error) {
		fi, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			return false, nil
		}
		if fi.IsDir() || fi.Mode()&0111 == 0 {
			return true, fmt.Errorf("%s: permission denied", p)
		}
		return true, nil
	}
	if strings.Contains(file, "/") {
		p := file
		if !path.IsAbs(p) {
			p = path.Join(dir, p)
		}
		exists, err := check(p)
		if err != nil {
			return "", ExitNotExecutable, err
		}
		if !exists {
			return "", ExitNotFound, fmt.Errorf("%s: no such file or directory", file)
		}
		return p, 0, nil
	}
	var denied error
	for _, d := range strings.Split(pathEnv, ":") {
		if d == "" {
			d = "."
		}
		if !path.IsAbs(d) {
			d = path.Join(dir, d)
		}
		p := path.Join(d, file)
		exists, err := check(p)
		if !exists {
			continue
		}
		if err != nil {
			denied = err
			continue
		}
		return p, 0, nil
	}
	if denied != nil {
		return "", ExitNotExecutable, denied
	}
	return "", ExitNotFound, fmt.Errorf("%s: executable file not found in $PATH", file)
}

// ResolvConf turns the kernel's DHCP results (/proc/net/pnp) into
// resolv.conf: its nameserver, domain and search lines.
func ResolvConf(pnp string) string {
	var b strings.Builder
	for _, line := range strings.Split(pnp, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "nameserver" || f[0] == "domain" || f[0] == "search") && f[1] != "0.0.0.0" {
			b.WriteString(strings.Join(f, " ") + "\n")
		}
	}
	return b.String()
}
