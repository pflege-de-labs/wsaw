#!/bin/sh
# Answers one question inside the wsaw image: does Chromium's own sandbox
# start? (Story 6.3, AC2 and AC3.)
#
# Run as the image's user, with the image's Chromium, and with no sandbox
# flag of any kind — so the browser either builds its namespace sandbox or
# refuses to start. "Started" is not taken as the answer on its own: a
# renderer is found and its user namespace compared with the browser's,
# because a sandboxed renderer lives in a namespace of its own and an
# unsandboxed one shares the browser's. That comparison is the evidence; a
# page that loaded is not.
#
# Exit 0 when a renderer is sandboxed, 1 when none is, 2 when the probe could
# not tell. CI runs it twice — under deploy/chromium-seccomp.json, where it
# must pass, and under the runtime's default profile, where it must fail —
# because a probe that has never been seen to fail proves nothing.
set -u

chrome=${WSAW_CHROME_PATH:-/usr/bin/chromium-browser}
log=$(mktemp)

"$chrome" --headless=new --disable-gpu --no-first-run \
	--remote-debugging-port=0 'data:text/html,<p>probe</p>' >"$log" 2>&1 &
browser=$!

self=$(readlink /proc/$browser/ns/user)
verdict=""
i=0
while [ $i -lt 60 ] && [ -z "$verdict" ]; do
	if ! kill -0 $browser 2>/dev/null; then
		verdict=exited
		break
	fi
	for p in /proc/[0-9]*; do
		cmd=$( { tr '\0' ' ' <"$p/cmdline"; } 2>/dev/null) || continue
		case "$cmd" in *--type=renderer*) ;; *) continue ;; esac
		case "$cmd" in *--no-sandbox*)
			echo "renderer $p runs with --no-sandbox"
			verdict=unsandboxed
			break ;;
		esac
		ns=$(readlink "$p/ns/user" 2>/dev/null) || continue
		if [ "$ns" != "$self" ]; then
			echo "renderer ${p#/proc/} is in $ns, the browser in $self"
			verdict=sandboxed
		else
			echo "renderer ${p#/proc/} shares the browser's user namespace $self"
			verdict=unsandboxed
		fi
		break
	done
	[ -n "$verdict" ] || { sleep 0.5; i=$((i + 1)); }
done

kill $browser 2>/dev/null
wait $browser 2>/dev/null

case "$verdict" in
sandboxed) exit 0 ;;
unsandboxed) exit 1 ;;
exited)
	echo "the browser exited before any renderer started:"
	grep -i -E 'sandbox|namespace|FATAL' "$log" | head -5
	exit 1 ;;
*)
	echo "no renderer appeared within 30s; the browser said:"
	tail -5 "$log"
	exit 2 ;;
esac
