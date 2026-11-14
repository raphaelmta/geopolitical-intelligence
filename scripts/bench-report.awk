# Turns the worker's JSON log of a benchmark run (scripts/bench-load.sh) into
# one tab-separated line per phase or step: collection, phase, step, rows,
# seconds. Reads only the fields the loader logs (names, counts, durations).
#
#   awk -f scripts/bench-report.awk worker.log
#
# Needs mktime (gawk or busybox awk).

# field(NAME): the value of a top-level JSON field of the current line.
function field(name,    re, s) {
	re = "\"" name "\":(\"[^\"]*\"|[^,}]*)"
	if (!match($0, re)) return ""
	s = substr($0, RSTART + length(name) + 3, RLENGTH - length(name) - 3)
	gsub(/^"|"$/, "", s)
	return s
}

# secs("1h2m3.5s" | "512ms"): a Go duration in seconds.
function secs(d,    t, n) {
	t = 0
	if (d ~ /^[0-9.]+ms$/) return sprintf("%.3f", substr(d, 1, length(d) - 2) / 1000)
	if (d ~ /^[0-9.]+(µs|us|ns)$/) return "0.000"
	if (match(d, /[0-9]+h/)) { t += substr(d, RSTART, RLENGTH - 1) * 3600; d = substr(d, RSTART + RLENGTH) }
	if (match(d, /[0-9]+m/)) { t += substr(d, RSTART, RLENGTH - 1) * 60; d = substr(d, RSTART + RLENGTH) }
	if (match(d, /[0-9.]+s/)) t += substr(d, RSTART, RLENGTH - 1)
	return sprintf("%.3f", t)
}

# epoch("2026-11-13T10:00:00.123Z"): seconds since 1970, with the fraction.
function epoch(ts,    a) {
	split(ts, a, /[-T:.Z+]/)
	return mktime(a[1] " " a[2] " " a[3] " " a[4] " " a[5] " " a[6]) + ("0." a[7])
}

function out(phase, step, rows, s) { printf "%d\t%s\t%s\t%s\t%s\n", col, phase, step, rows, s }

{
	msg = field("msg")
	if (msg == "") next
	now = epoch(field("time"))
}
msg == "collection starting" { col++; t_start = now; t_mark = now; chunk_prev = 0; next }
msg == "receita: downloaded" { downloads++; next }
msg == "receita: parse and copy done" {
	p = secs(field("elapsed"))
	out("fetch", "list and download", downloads, sprintf("%.3f", now - t_start - p))
	out("parse", "parse and copy", field("rows_read"), p)
	downloads = 0; next
}
msg == "receita: load step" { out(field("phase"), field("step"), field("rows"), secs(field("elapsed"))); next }
msg == "receita: relationship chunks" {
	e = secs(field("elapsed"))
	out("relationships", "chunks to " field("done"), field("relationships_inserted") "+" field("relationships_seen"), sprintf("%.3f", e - chunk_prev))
	chunk_prev = e; next
}
msg == "receita: load phase done" { out(field("phase"), "PHASE", "", secs(field("elapsed"))); next }
msg == "receita: load done" { out("load", "TOTAL (parse to analyze)", "", secs(field("elapsed"))); t_mark = now; next }
msg == "unseen claims closed" { out("close unseen", "PHASE", field("closed"), sprintf("%.3f", now - t_mark)); t_mark = now; next }
msg == "identities resolved" {
	out("resolve", "PHASE", field("auto_merged") " moved, " field("auto_merged_groups") " groups, " field("proposed") " proposed",
		sprintf("%.3f", now - t_mark))
	t_mark = now; next
}
msg == "search rebuilt" { out("search", "PHASE", "", sprintf("%.3f", field("duration") / 1e9)); next }
msg == "collection finished" { out("collection", "TOTAL", "", sprintf("%.3f", now - t_start)); next }
msg == "collection failed" { out("collection", "FAILED", "", sprintf("%.3f", now - t_start)); next }
