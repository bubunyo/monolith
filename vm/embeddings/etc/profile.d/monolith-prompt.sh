# Coloured prompt for login shells: user@host in green (red for root), the
# directory in blue. Plain when NO_COLOR is set or the terminal is dumb.
if [ "$(id -u)" = 0 ]; then
	who=31
else
	who=32
fi
if [ -z "$NO_COLOR" ] && [ "$TERM" != dumb ]; then
	e=$(printf '\033')
	PS1="\[$e[1;${who}m\]\u@\h\[$e[0m\]:\[$e[34m\]"'\w'"\[$e[0m\]"'\$ '
	unset e
else
	PS1='\u@\h:\w\$ '
fi
unset who
export PS1
