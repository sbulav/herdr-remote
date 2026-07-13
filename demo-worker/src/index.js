export default {
  async fetch(request, env) {
    if (request.method === 'POST') {
      return new Response('ok\n', { headers: { 'Access-Control-Allow-Origin': '*' } });
    }
    if (request.method === 'OPTIONS') {
      return new Response(null, { headers: { 'Access-Control-Allow-Origin': '*', 'Access-Control-Allow-Headers': '*' } });
    }

    const upgrade = request.headers.get('Upgrade');
    if (upgrade !== 'websocket') {
      return new Response('herdr-remote demo relay. Connect via WebSocket.', {
        headers: { 'Access-Control-Allow-Origin': '*' }
      });
    }

    const [client, server] = Object.values(new WebSocketPair());
    server.accept();

    const agents = [
      { pane_id: 'demo:1', agent: 'claude', status: 'working', project: 'phoenix-api', cwd: '/dev/phoenix-api', host: 'local' },
      { pane_id: 'demo:2', agent: 'codex', status: 'idle', project: 'nova-ingest', cwd: '/dev/nova-ingest', host: 'local' },
      { pane_id: 'demo:3', agent: 'kiro', status: 'blocked', project: 'orbit-ui', cwd: '/dev/orbit-ui', host: 'local' },
      { pane_id: 'demo:4', agent: 'grok', status: 'working', project: 'atlas-core', cwd: '/dev/atlas-core', host: 'remote-1' },
      { pane_id: 'demo:5', agent: 'copilot', status: 'idle', project: 'delta-sync', cwd: '/dev/delta-sync', host: 'local' },
      { pane_id: 'demo:6', agent: 'claude', status: 'working', project: 'nebula-ml', cwd: '/dev/nebula-ml', host: 'remote-2' },
    ];

    const claudePrompt = `Ask rule Bash(git add *) overrides auto mode for this command.
 /permissions to let auto mode decide

 Do you want to proceed?
 ❯ 1. Yes
   2. No`;
    const opencodePrompt = `△ Permission required
  Bash · git status
  Allow once   Allow always   Reject
  ↔ select   enter confirm   esc dismiss`;

    server.send(JSON.stringify({ type: 'agents', agents }));
    server.send(JSON.stringify({
      type: 'blocked', pane_id: 'demo:3', agent: 'kiro', project: 'orbit-ui',
      prompt: claudePrompt, host: 'local',
      options: ['1. Yes', '2. No']
    }));
    // Also show an OpenCode-style permission on another agent
    agents[1].status = 'blocked';
    server.send(JSON.stringify({
      type: 'blocked', pane_id: 'demo:2', agent: 'codex', project: 'nova-ingest',
      prompt: opencodePrompt, host: 'local',
      options: ['Allow once', 'Allow always', 'Reject']
    }));

    let interval = setInterval(() => {
      const idx = Math.floor(Math.random() * agents.length);
      const statuses = ['working', 'idle', 'blocked'];
      agents[idx].status = statuses[Math.floor(Math.random() * statuses.length)];
      try {
        server.send(JSON.stringify({ type: 'agents', agents }));
        if (agents[idx].status === 'blocked') {
          const useOpenCode = agents[idx].agent === 'codex' || agents[idx].agent === 'opencode' || Math.random() < 0.4;
          server.send(JSON.stringify({
            type: 'blocked', pane_id: agents[idx].pane_id, agent: agents[idx].agent,
            project: agents[idx].project,
            prompt: useOpenCode ? opencodePrompt : claudePrompt,
            host: agents[idx].host,
            options: useOpenCode ? ['Allow once', 'Allow always', 'Reject'] : ['1. Yes', '2. No']
          }));
        }
      } catch { clearInterval(interval); }
    }, 5000);

    server.addEventListener('message', (event) => {
      try {
        const msg = JSON.parse(event.data);
        if (msg.type === 'read_pane') {
          server.send(JSON.stringify({
            type: 'pane_content', pane_id: msg.pane_id,
            content: `$ herdr agent session\n\n[demo mode -- read-only preview]\n\nAgent: ${msg.pane_id.split(':')[1]}\nProject: ${agents.find(a => a.pane_id === msg.pane_id)?.project || 'unknown'}\n\n  Compiled successfully\n  Running tests...\n\n  PASS src/index.test.ts\n  PASS src/utils.test.ts\n\nAll tests passed.`
          }));
        } else if (msg.type === 'respond') {
          const a = agents.find(x => x.pane_id === msg.pane_id);
          if (a) a.status = 'working';
          server.send(JSON.stringify({ type: 'agents', agents }));
        }
      } catch {}
    });

    server.addEventListener('close', () => clearInterval(interval));
    return new Response(null, { status: 101, webSocket: client });
  }
};
