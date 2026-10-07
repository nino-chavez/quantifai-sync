# Combine captured POST bodies into one canonical batch: distinct units,
# sessions and messages each sorted, so two clients' output can be diffed.
import json, sys
units, sessions, messages = {}, [], []
for line in open(sys.argv[1]):
    b = json.loads(line)
    for u in b.get('unitsOfWork', []): units[u['projectPath']] = u
    sessions += b.get('sessions', []); messages += b.get('messages', [])
# A session repeated across batches must be identical; keep one.
dedup = {}
for s in sessions:
    k = (s['projectPath'], s['sessionId'])
    if k in dedup and dedup[k] != s: sys.exit(f'conflicting rows for session {k}')
    dedup[k] = s
out = {'unitsOfWork': sorted(units.values(), key=lambda u: u['projectPath']),
       'sessions': sorted(dedup.values(), key=lambda s: (s['projectPath'], s['sessionId'])),
       'messages': sorted(messages, key=lambda m: (m['messageId'], m['sessionId'], m['timestamp']))}
json.dump(out, sys.stdout, indent=1, sort_keys=True); print()
