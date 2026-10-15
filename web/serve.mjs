import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { resolve, extname, sep } from 'node:path';

const root = resolve('public');
const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.svg': 'image/svg+xml', '.woff': 'font/woff', '.woff2': 'font/woff2' };
const server = createServer(async (request, response) => {
  response.setHeader('X-Content-Type-Options', 'nosniff');
  if (!['GET', 'HEAD'].includes(request.method)) { response.writeHead(405, { Allow: 'GET, HEAD' }).end(); return; }
  try {
    const path = decodeURIComponent(new URL(request.url, 'http://localhost').pathname);
    let target = resolve(root, '.' + path);
    if (target !== root && !target.startsWith(root + sep)) { response.writeHead(400).end(); return; }
    let body;
    try { body = await readFile(target); } catch {
      if (extname(path)) { response.writeHead(404).end(); return; }
      target = resolve(root, 'index.html');
      body = await readFile(target);
    }
    response.setHeader('Content-Type', types[extname(target)] ?? 'application/octet-stream');
    response.setHeader('Cache-Control', extname(target) === '.html' ? 'no-cache' : 'public, max-age=3600');
    response.end(request.method === 'HEAD' ? undefined : body);
  } catch { response.writeHead(400).end(); }
});
server.listen(8080, '0.0.0.0');
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => server.close());
