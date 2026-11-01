import { test, expect } from '../harness/fixtures';

const CURATED = [
  'enqueue_review', 'job_log', 'job_status', 'list_agents', 'list_jobs', 'list_prs', 'list_reviews', 'read_review',
];

test.describe('P0 · MCP', () => {
  test('initialize + tools/list: the curated surface, no publishing tool', async ({ bazel }) => {
    const init = await bazel.mcp('initialize', {
      protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'e2e', version: '0' },
    });
    expect(init.status).toBe(200);
    expect(init.json.result.serverInfo.name).toBe('bazel');
    expect(init.json.result.capabilities).toHaveProperty('tools');

    const list = await bazel.mcp('tools/list', {}, 2);
    expect(list.status).toBe(200);
    expect(list.json.id).toBe(2);
    const names = list.json.result.tools.map((t: any) => t.name).sort();
    expect(names).toEqual(CURATED);
    for (const t of list.json.result.tools) {
      expect(t.description, t.name).toBeTruthy();
      expect(t.inputSchema?.type, t.name).toBe('object');
    }
  });

  test('tools/call list_prs answers from the fake gh and carries web_ui', async ({ bazel }) => {
    const res = await bazel.mcp('tools/call', { name: 'list_prs', arguments: { repo: 'acme/web-app' } });
    expect(res.status).toBe(200);
    const out = res.json.result.structuredContent;
    expect(out.web_ui).toBe(bazel.url + '/');
    expect(JSON.stringify(out)).toContain('Dark mode toggle in settings');
    expect(JSON.stringify(out)).not.toContain('Add rate limiter');
  });

  test('a POST to /mcp from another origin is refused', async ({ bazel }) => {
    const res = await fetch(`${bazel.url}/mcp`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Origin: 'https://evil.example' },
      body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list' }),
    });
    expect(res.status).toBe(403);
  });
});

test.describe('P0 · MCP with mcp_allow_publish', () => {
  test.use({ bazelOptions: { config: { mcp_allow_publish: true } } });

  test('tools/list adds publish_review', async ({ bazel }) => {
    const list = await bazel.mcp('tools/list');
    const names = list.json.result.tools.map((t: any) => t.name).sort();
    expect(names).toEqual([...CURATED, 'publish_review'].sort());
  });
});
