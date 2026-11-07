#!/usr/bin/env python3
"""jget.py - print one value from a JSON file by dotted path (e2e.sh helper; no jq on the host).

usage: jget.py FILE PATH        FILE may be '-' for stdin; PATH like "urls.page", "clips.0.id" or "" (whole document)
       jget.py --selftest

Output rules (one line, LF-terminated, UTF-8, never CRLF):
  null or a missing path  -> empty line          (so `[ -z "$(jget f views)" ]` means "absent or null")
  string                  -> the raw string
  true / false            -> true / false
  number                  -> JSON number (6000, 1.5)
  object / array          -> compact JSON
Exit 0 in all of those cases; exit 1 on an unreadable file, invalid JSON or bad usage (message on stderr).
"""
import json
import sys

MISSING = object()


def lookup(doc, path):
    """Walk PATH through DOC. Numeric segments index lists; anything else is a dict key."""
    cur = doc
    for part in [p for p in path.split('.') if p != '']:
        if isinstance(cur, dict):
            if part not in cur:
                return MISSING
            cur = cur[part]
        elif isinstance(cur, list):
            try:
                idx = int(part)
            except ValueError:
                return MISSING
            if not -len(cur) <= idx < len(cur):
                return MISSING
            cur = cur[idx]
        else:
            return MISSING
    return cur


def render(value):
    if value is MISSING or value is None:
        return ''
    if isinstance(value, bool):
        return 'true' if value else 'false'
    if isinstance(value, str):
        return value
    if isinstance(value, (int, float)):
        return json.dumps(value)
    return json.dumps(value, separators=(',', ':'), ensure_ascii=False)


def emit(text):
    # Bytes, not text: Windows Python would otherwise write CRLF and `$(...)` in bash keeps the CR.
    sys.stdout.buffer.write((text + '\n').encode('utf-8'))
    sys.stdout.buffer.flush()


def selftest():
    doc = json.loads('{"a":{"b":[10,{"c":"x"},null,true,false,1.5]},"s":"str","n":null,"z":0,"u":"caf\\u00e9"}')
    cases = [
        ('a.b.0', '10'), ('a.b.1.c', 'x'), ('a.b.2', ''), ('a.b.3', 'true'), ('a.b.4', 'false'), ('a.b.5', '1.5'),
        ('a.b.-1', '1.5'), ('a.b.9', ''), ('a.x', ''), ('s', 'str'), ('s.x', ''), ('n', ''), ('z', '0'), ('u', 'café'),
        ('missing', ''), ('a.b.1', '{"c":"x"}'), ('', None),
    ]
    for path, want in cases:
        got = render(lookup(doc, path))
        if want is None:
            want = json.dumps(doc, separators=(',', ':'), ensure_ascii=False)
        if got != want:
            sys.stderr.write('jget selftest FAILED: path %r -> %r, want %r\n' % (path, got, want))
            return 1
    emit('jget selftest ok (%d cases)' % len(cases))
    return 0


def main(argv):
    if len(argv) == 2 and argv[1] == '--selftest':
        return selftest()
    if len(argv) != 3:
        sys.stderr.write(__doc__)
        return 1
    path_arg, dotted = argv[1], argv[2]
    try:
        if path_arg == '-':
            doc = json.loads(sys.stdin.buffer.read().decode('utf-8'))
        else:
            with open(path_arg, 'rb') as fh:
                doc = json.loads(fh.read().decode('utf-8'))
    except (OSError, ValueError) as exc:
        sys.stderr.write('jget: %s: %s\n' % (path_arg, exc))
        return 1
    emit(render(lookup(doc, dotted)))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
