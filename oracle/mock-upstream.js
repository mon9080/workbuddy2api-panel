// oracle/mock-upstream.js — 校准用 mock 上游。
// 记录官方 CLI 的全部出站请求（含 gzip 解码），对已知端点给最小可用响应，未知端点 404 记录。
// 用法: node oracle/mock-upstream.js <port> <captureDir>
'use strict';
const http = require('http');
const zlib = require('zlib');
const fs = require('fs');
const path = require('path');

const PORT = Number(process.argv[2] || 8799);
const OUT = process.argv[3] || path.join(__dirname, 'captures');
fs.mkdirSync(OUT, { recursive: true });
const LOG = path.join(OUT, 'requests.jsonl');
fs.writeFileSync(LOG, ''); // 每次启动清零，保证基线干净

const MODELS = {
  object: 'list',
  data: [
    { id: 'deepseek-v4.1-flash', object: 'model', owned_by: 'tencent' },
    { id: 'glm-5.3', object: 'model', owned_by: 'tencent' },
  ],
};

function sse(res, events) {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
  for (const e of events) res.write(`data: ${JSON.stringify(e)}\n\n`);
  res.write('data: [DONE]\n\n');
  res.end();
}

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    const raw = Buffer.concat(chunks);
    let body = null;
    let bodyErr = null;
    if (raw.length) {
      try {
        body = (req.headers['content-encoding'] === 'gzip')
          ? zlib.gunzipSync(raw).toString('utf8')
          : raw.toString('utf8');
        try { body = JSON.parse(body); } catch { /* 保留文本 */ }
      } catch (e) { bodyErr = String(e); }
    }
    const entry = {
      ts: new Date().toISOString(),
      method: req.method,
      url: req.url,
      headers: req.headers,
      body,
      bodyErr,
      gzipped: req.headers['content-encoding'] === 'gzip' && raw.length > 0,
    };
    fs.appendFileSync(LOG, JSON.stringify(entry) + '\n');
    console.log(`[${entry.ts}] ${req.method} ${req.url} gz=${entry.gzipped}`);

    const u = req.url.split('?')[0];
    if (req.method === 'GET' && /models?$/.test(u)) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      return res.end(JSON.stringify(MODELS));
    }
    if (req.method === 'POST' && /chat|completions|messages/.test(u)) {
      return sse(res, [
        { id: 'mock-1', object: 'chat.completion.chunk', choices: [{ index: 0, delta: { role: 'assistant', content: 'OK' }, finish_reason: null }] },
        { id: 'mock-1', object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }], usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 } },
      ]);
    }
    res.writeHead(404, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'mock: not implemented', path: u }));
  });
});
server.listen(PORT, '127.0.0.1', () => console.log(`mock upstream on 127.0.0.1:${PORT}, logging -> ${LOG}`));
