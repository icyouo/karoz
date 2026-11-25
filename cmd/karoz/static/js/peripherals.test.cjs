const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function makeElement(tagName) {
  const el = {
    tagName: String(tagName || 'div').toUpperCase(),
    children: [],
    className: '',
    dataset: {},
    style: {},
    attributes: {},
    _innerHTML: '',
    textContent: '',
    value: '',
    hidden: false,
    disabled: false,
    classList: {
      toggle(name, force) {
        const set = new Set((el.className || '').split(/\s+/).filter(Boolean));
        const has = set.has(name);
        const next = force === undefined ? !has : !!force;
        if (next) set.add(name);
        else set.delete(name);
        el.className = Array.from(set).join(' ');
      },
      add(name) { this.toggle(name, true); },
      remove(name) { this.toggle(name, false); },
      contains(name) { return (el.className || '').split(/\s+/).includes(name); },
    },
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return this.attributes[k]; },
    addEventListener() {},
    appendChild(child) { this.children.push(child); return child; },
    prepend(child) { this.children.unshift(child); return child; },
    querySelector(sel) {
      if (sel === 'iframe' && this._innerHTML.includes('<iframe')) {
        if (!this._iframe) {
          const sandboxMatch = this._innerHTML.match(/sandbox="([^"]*)"/);
          this._iframe = makeElement('iframe');
          if (sandboxMatch) this._iframe.setAttribute('sandbox', sandboxMatch[1]);
        }
        return this._iframe;
      }
      if (sel === 'button') return makeElement('button');
      return null;
    },
    querySelectorAll() { return []; },
    get options() { return this._options || []; },
    set options(v) { this._options = v; },
    set innerHTML(val) {
      this._innerHTML = String(val);
      this._iframe = null;
      if (val === '') {
        this.children = [];
        this._options = [];
      }
    },
    get innerHTML() { return this._innerHTML; },
  };
  return el;
}

function loadStudioScripts() {
  const elements = new Map();
  const byId = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement('div'));
    return elements.get(id);
  };
  const ctx = {
    URL,
    Blob: class {},
    KarozContextTokens: { estimateContextTokens: () => 0 },
    console,
    setTimeout,
    clearTimeout,
    document: {
      getElementById: byId,
      createElement: (tag) => makeElement(tag),
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    location: { protocol: 'http:', host: '127.0.0.1:8080' },
    localStorage: { getItem: () => '', setItem: () => {} },
    escapeHTML: (s) => String(s || '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c])),
    currentAgentWorking: () => false,
    renderContextTokenUsage: () => {},
    agentWorking: () => false,
    renderAgentWorkingState: () => {},
    renderMarkdown: (s) => s,
    notify: () => {},
  };
  vm.createContext(ctx);
  for (const name of ['core.js', 'agents.js', 'panels.js', 'chat-stream.js']) {
    const code = fs.readFileSync(path.join(__dirname, name), 'utf8');
    vm.runInContext(code, ctx, { filename: name });
  }
  const state = vm.runInContext('state', ctx);
  return { ctx, state, byId };
}

test('composite provider::model selector disambiguates shared model ids and supports legacy values', () => {
  const { ctx, state, byId } = loadStudioScripts();
  const select = byId('agentModel');
  select.appendChild = function (group) {
    this.children.push(group);
    this._options = (this._options || []).concat(group.children || []);
  };
  state.providers = [
    { id: 'codex-direct', display_name: 'Codex', available: true, models: [{ id: 'gpt-5', provider: 'codex-direct', display_name: 'GPT-5 Codex', effort_levels: ['medium'] }] },
    { id: 'openai-compat', display_name: 'OpenAI', available: true, models: [{ id: 'gpt-5', provider: 'openai-compat', display_name: 'GPT-5 Compat', effort_levels: ['high'] }] },
  ];
  ctx.renderModelCatalog();

  const opts = select.options;
  assert.equal(opts.length, 2);
  assert.equal(opts[0].value, 'codex-direct::gpt-5');
  assert.equal(opts[1].value, 'openai-compat::gpt-5');

  select.value = 'openai-compat::gpt-5';
  const desc = ctx.selectedModelDescriptor();
  assert.equal(desc.provider.id, 'openai-compat');
  assert.equal(desc.model.id, 'gpt-5');

  // Legacy stored agent with model='gpt-5' and provider='openai-compat' restores to composite option
  state.agent = { id: 'a1', provider: 'openai-compat', model: 'gpt-5', thinking_effort: 'high' };
  ctx.restoreAgentModelSettings();
  assert.equal(select.value, 'openai-compat::gpt-5');
});

test('renderAgents groups team agents under collapsible group headers', () => {
  const { ctx, state, byId } = loadStudioScripts();
  state.project = { id: 'p1', name: 'Demo' };
  state.groups = [{ id: 'grp-build', name: 'Build Lane' }];
  state.agents = [
    { id: 'karoz', name: 'karoz', nickname: 'Karoz' },
    { id: 'lead', name: 'implementation-lead', group_id: 'grp-build', group_role: 'lead' },
    { id: 'rev', name: 'review-critic', group_id: 'grp-build', group_role: 'reviewer' },
  ];
  ctx.renderAgents();
  const box = byId('agentList');
  assert.equal(box.children.length, 3); // karoz + group header + group wrap
  assert.equal(box.children[1].className, 'agent-group-header');
  assert.equal(box.children[1].getAttribute('aria-expanded'), 'true');
  assert.equal(box.children[2].children.length, 2);

  // Clicking group header collapses members cleanly
  box.children[1].onclick();
  assert.equal(state.collapsedGroups['grp-build'], true);
  assert.equal(box.children.length, 2); // karoz + collapsed header
  assert.equal(box.children[1].getAttribute('aria-expanded'), 'false');
});

test('renderSkillSuggest shows explicit empty state instead of silently hiding', () => {
  const { ctx, state, byId } = loadStudioScripts();
  state.skillSuggest = { open: true, items: [], active: -1, trigger: { prefix: '$' } };
  ctx.renderSkillSuggest();
  const box = byId('skillSuggest');
  assert.equal(box.classList.contains('open'), true);
  assert.equal(box.children.length, 1);
  assert.equal(box.children[0].className, 'skill-suggest-empty');
  assert.equal(box.children[0].textContent, 'No matching skills');
});

test('renderPreviewPane omits allow-same-origin and restricts live URLs to loopback', () => {
  const { ctx, state, byId } = loadStudioScripts();
  assert.equal(ctx.isSafeLoopbackPreviewURL('http://localhost:5173/app'), true);
  assert.equal(ctx.isSafeLoopbackPreviewURL('http://127.0.0.1:3000'), true);
  assert.equal(ctx.isSafeLoopbackPreviewURL('https://evil.example.com'), false);
  assert.equal(ctx.isSafeLoopbackPreviewURL('javascript:alert(1)'), false);

  const body = byId('sidePaneBody');
  state.preview = { filename: 'index.html', mime_type: 'text/html', content: '<h1>Hi</h1>' };
  ctx.renderPreviewPane(body);
  const iframe = body.querySelector('iframe');
  assert.ok(iframe);
  const sandbox = iframe.getAttribute('sandbox');
  assert.equal(sandbox.includes('allow-scripts'), true);
  assert.equal(sandbox.includes('allow-forms'), true);
  assert.equal(sandbox.includes('allow-same-origin'), false);
});

test('renderAgentAttachments creates image thumbnail URLs and clearAgentAttachments revokes them', () => {
  const { ctx, state, byId } = loadStudioScripts();
  const created = [];
  const revoked = [];
  ctx.URL = {
    createObjectURL(file) {
      const url = 'blob:mock/' + file.name;
      created.push(url);
      return url;
    },
    revokeObjectURL(url) {
      revoked.push(url);
    },
  };
  ctx.formatBytes = (n) => `${n} B`;
  state.agentAttachments = [{ name: 'screen.png', size: 120, type: 'image/png' }];
  ctx.renderAgentAttachments();
  const row = byId('agentAttachmentRow');
  assert.equal(row.hidden, false);
  assert.equal(created.length, 1);
  assert.equal(row.children[0].innerHTML.includes('attachment-thumb'), true);

  ctx.clearAgentAttachments();
  assert.deepEqual(revoked, ['blob:mock/screen.png']);
  assert.equal(state.agentAttachments.length, 0);
});
