#!/usr/bin/env python3
"""Writes testdata/parity/projects: synthetic Claude Code session files that
exercise every rule the Go collector must share with the reference importer
(apps/app/scripts/import-claude-jsonl.ts). Regenerate golden.json from this
tree with the real importer, as described in README.md beside this file."""
import json, os, shutil

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'projects')
shutil.rmtree(root, ignore_errors=True)

def asst(sid, uuid, ts, model, usage, cwd=None, entry=None, tools=(), **extra):
    msg = {'role': 'assistant', 'content': [{'type': 'text', 'text': 'x'}] +
           [{'type': 'tool_use', 'id': f't{i}', 'name': t, 'input': {}} for i, t in enumerate(tools)]}
    if model is not None: msg['model'] = model
    if usage is not None: msg['usage'] = usage
    r = {'type': 'assistant', 'sessionId': sid, 'uuid': uuid, 'timestamp': ts, 'message': msg}
    if cwd is not None: r['cwd'] = cwd
    if entry is not None: r['entrypoint'] = entry
    r.update(extra)
    return r

def user(sid, uuid, ts, cwd=None):
    r = {'type': 'user', 'sessionId': sid, 'uuid': uuid, 'timestamp': ts,
         'message': {'role': 'user', 'content': 'a prompt that mentions "usage" in passing'}}
    if cwd: r['cwd'] = cwd
    return r

def u(i, o, cr=0, cc=0):
    return {'input_tokens': i, 'output_tokens': o, 'cache_read_input_tokens': cr, 'cache_creation_input_tokens': cc}

def write(rel, records, raw_lines=()):
    path = os.path.join(root, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w') as f:
        for r in records:
            f.write(json.dumps(r, separators=(',', ':')) + '\n')
        for line in raw_lines:
            f.write(line + '\n')

A = '-Users-test-repo-alpha'
# Walk order puts the s1/ directory before s1.jsonl, so the subagent file's
# cwd is the project's first cwd, and its records start session s1.
write(f'{A}/s1/subagents/agent-1.jsonl', [
    asst('s1', 'a1-sub-1', '2026-10-01T10:05:00.000Z', 'claude-haiku-4-5-20251001', u(1203, 377, 15011, 901),
         cwd='/Users/test/repo-alpha', entry='sdk-ts', tools=['Grep', 'Read']),
    asst('s1', 'a1-dup', '2026-10-01T10:06:00.000Z', 'claude-haiku-4-5-20251001', u(7, 3), cwd='/Users/test/repo-alpha/sub'),
])
write(f'{A}/s1.jsonl', [
    user('s1', 'u1', '2026-10-01T09:59:00.000Z', cwd='/Users/test/repo-alpha'),
    asst('s1', 'a1-1', '2026-10-01T10:00:00.000Z', 'claude-sonnet-4-6', u(3, 1250, 22113, 4517), cwd='/Users/test/repo-alpha', entry='cli', tools=['Bash']),
    asst('s1', 'a1-2', '2026-10-01T10:01:00.000Z', 'claude-opus-4-6', u(11, 999, 31337, 2), cwd='/Users/test/repo-alpha', tools=['Bash', 'Edit']),
    asst('s1', 'a1-3', '2026-10-01T10:02:00.000Z', 'claude-opus-4-6', u(1, 1), cwd='/Users/test/repo-alpha'),
    # Same uuid as a subagent record: one messages row survives server-side,
    # but the session totals count both, as the reference importer does.
    asst('s1', 'a1-dup', '2026-10-01T10:06:00.000Z', 'claude-haiku-4-5-20251001', u(7, 3), cwd='/Users/test/repo-alpha'),
    asst('s1', 'no-usage', '2026-10-01T10:03:00.000Z', 'claude-opus-4-6', None),
    {'type': 'assistant', 'sessionId': 's1', 'uuid': 'null-usage', 'timestamp': '2026-10-01T10:03:30.000Z', 'message': {'model': 'claude-opus-4-6', 'usage': None}},
    asst('', 'empty-session', '2026-10-01T10:04:00.000Z', 'claude-opus-4-6', u(5, 5)),
    asst('s1', 'a1-nulltok', '2026-09-30T23:59:59.999Z', 'claude-sonnet-4-6',
         {'input_tokens': None, 'output_tokens': 42, 'cache_read_input_tokens': 7}),
], raw_lines=['{"type":"assistant","sessionId":"s1","uuid":"torn"', '   '])
write(f'{A}/s2.jsonl', [
    asst('s2', 'a2-1', '2026-10-02T08:00:00.000Z', None, u(100, 200, 300, 400), cwd='/Users/test/repo-alpha/.claude/worktrees/agent-x'),
    asst('s2', 'a2-2', '2026-10-02T08:01:00.000Z', 'some-future-model', u(1, 2, 3, 4), entry='', tools=['WebFetch', 'WebFetch']),
    asst('s2', 'a2-3', '2026-10-02T07:59:00.000Z', 'Claude-HAIKU-5', u(9, 8, 7, 6)),
])

B = '-Users-test-no-cwd'
write(f'{B}/s3.jsonl', [
    asst('s3', 'b3-1', '2026-10-03T12:00:00.000Z', 'claude-sonnet-4-6', u(10, 20)),
    asst('s3', 'b3-2', '2026-10-03T12:00:01.000Z', 'claude-opus-4-6', u(10, 20)),
])

C = '-Users-test-repo-gamma--claude-worktrees-abc'
write(f'{C}/s4.jsonl', [
    asst('s4', 'c4-1', '2026-10-04T01:00:00.000Z', 'claude-opus-4-6', u(123456, 654321, 9876543, 12345),
         cwd='/Users/test/repo-gamma/.claude/worktrees/abc', entry='cli', tools=['Task']),
    asst('s4', 'c4-2', '2026-10-04T01:00:00.000Z', 'claude-opus-4-6', u(1, 1, 1, 1), cwd='/Users/other'),
])
# A session id that also appears in another project directory.
write(f'{C}/shared.jsonl', [
    asst('s3', 'c3-1', '2026-10-04T02:00:00.000Z', 'claude-sonnet-4-6', u(3, 3)),
])

os.makedirs(os.path.join(root, '-Users-test-empty'), exist_ok=True)
with open(os.path.join(root, 'stray-top-level.jsonl'), 'w') as f:
    f.write(json.dumps(asst('s9', 'top-1', '2026-10-05T00:00:00.000Z', 'claude-opus-4-6', u(1, 1))) + '\n')
