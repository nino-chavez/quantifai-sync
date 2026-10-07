# Minimal stand-in for POST /api/v1/ingest: records every body and answers
# with counts computed the way processIngestBatch does (no dedup state).
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
out = open(sys.argv[2], 'a')
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['content-length'])))
        out.write(json.dumps(body) + '\n'); out.flush()
        res = {
            'unitsOfWork': len({u['projectPath'] for u in body.get('unitsOfWork', [])}),
            'sessions': len(body.get('sessions', [])),
            'messages': {'accepted': len(body.get('messages', [])), 'errors': 0},
            'gitEvents': {'accepted': len(body.get('gitEvents', [])), 'linked': 0, 'deterministic': 0},
        }
        data = json.dumps(res).encode()
        self.send_response(200); self.send_header('content-type', 'application/json'); self.end_headers(); self.wfile.write(data)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
