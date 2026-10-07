# Parity fixture

`golden.json` is what the server repo's reference importer
(`apps/app/scripts/import-claude-jsonl.ts`) POSTs for `projects/`.
`TestParityWithReferenceImporter` requires the Go collector to produce the
same units, sessions and messages, with floats compared exactly. The test
is how the shipper and the importer are kept writing identical rows.

To regenerate after changing the fixture or the importer, from this
directory (`QUANTIFAI` is the server repo checkout):

```
python3 make_fixture.py
python3 capture.py 18871 /tmp/ts-capture.jsonl &
QUANTIFAI_API_URL=http://127.0.0.1:18871 QUANTIFAI_API_KEY=x \
  "$QUANTIFAI/node_modules/.bin/tsx" "$QUANTIFAI/apps/app/scripts/import-claude-jsonl.ts" --dir "$PWD/projects"
kill %1
python3 combine.py /tmp/ts-capture.jsonl > golden.json
```

`capture.py` stands in for the ingest endpoint and records every body;
`combine.py` merges the bodies into one sorted batch.
