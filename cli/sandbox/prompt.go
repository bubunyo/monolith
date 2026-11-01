package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bubunyo/monolith/vm"
)

// shellRC is the sandbox shell's rc file. The prompt is bold yellow text
// with the location relative to the repo, "lith:some-repo/src",
// or the absolute path outside it, "lith:/tmp", then a marker when the
// network is restricted. bash and busybox ash get colour (their \[ \] keep
// line editing right); other shells, NO_COLOR and dumb terminals get plain
// text.
func shellRC(repo, network string) string {
	var marker, plainMarker string
	switch network {
	case vm.SandboxNetworkNone:
		marker, plainMarker = `\[$e[1;31m\]⊘net\[$e[0m\] `, "⊘net "
	case vm.SandboxNetworkAllow:
		marker, plainMarker = `\[$e[1;36m\]allow\[$e[0m\] `, "allow "
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("deactivate() { exit 0; }")
	w("__lith_repo=%s", shellQuote(repo))
	w(`__lith_where() {`)
	w(`	case $PWD in`)
	w(`	"$__lith_repo" | "$__lith_repo"/*) printf '%%s' %s"${PWD#"$__lith_repo"}" ;;`, shellQuote("lith:"+promptSafe(filepath.Base(repo))))
	w(`	*) printf 'lith:%%s' "$PWD" ;;`)
	w(`	esac`)
	w(`}`)
	w(`case "$BASH_VERSION:$(readlink /proc/$$/exe 2>/dev/null)" in`)
	w(`?*:*|*busybox*) fancy=1 ;;`)
	w(`*) fancy= ;;`)
	w("esac")
	w(`if [ -n "$fancy" ] && [ -z "$NO_COLOR" ] && [ "$TERM" != dumb ]; then`)
	w(`	e=$(printf '\033')`)
	w(`	PS1="\[$e[1;33m\]"'$(__lith_where)'"\[$e[0m\] %s"'\$ '`, marker)
	w("	unset e")
	w(`elif [ -n "$fancy" ]; then`)
	w(`	PS1='($(__lith_where)) %s\$ '`, plainMarker)
	w("else")
	// No prompt escapes here (dash).
	w(`	PS1='($(__lith_where)) %s# '`, plainMarker)
	w("fi")
	w("unset fancy")
	return b.String()
}

// promptSafe keeps a name from being interpreted by the prompt (backslash
// escapes, $, backticks): anything unusual becomes '-'.
func promptSafe(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '+', r == '@':
			return r
		}
		return '-'
	}, name)
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
