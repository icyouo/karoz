const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'context-tokens.js'), 'utf8');
const sandbox = { window: {} };
vm.runInNewContext(source, sandbox);
const tokens = sandbox.window.KarozContextTokens;

test('empty chat has zero context tokens', () => {
  assert.equal(tokens.estimateContextTokens([], [], ''), 0);
});

test('current turn counts immediately after send and while streaming', () => {
  const history = [{ role: 'assistant', intent: 'response', body: 'Previous response with enough text to establish context.' }];
  let turn = tokens.beginCurrentTurn('Please review this implementation in detail.');
  const afterSend = tokens.estimateContextTokens(history, turn, '');
  turn = tokens.updateAssistantTurn(turn, 'I will inspect the implementation and report the issues I find.');
  assert.ok(tokens.estimateContextTokens(history, turn, '') > afterSend);
});

test('tool events increase the current turn context and final refresh replaces the local turn', () => {
  const history = [{ role: 'user', intent: 'message', body: 'Start the review.' }];
  let turn = tokens.beginCurrentTurn('Inspect the repository.');
  turn = tokens.updateAssistantTurn(turn, 'I will check the relevant files.');
  const beforeTools = tokens.estimateContextTokens(history, turn, '');
  turn = tokens.appendTurnEvent(turn, 'tool_call', 'repo_search', '{"query":"token accounting"}');
  turn = tokens.appendTurnEvent(turn, 'tool_result', 'repo_search', '{"matches":["chat-stream.js","core.js"]}');
  const duringTools = tokens.estimateContextTokens(history, turn, '');
  assert.ok(duringTools > beforeTools);

  const refreshedHistory = tokens.compactMessages(history, turn);
  assert.equal(tokens.estimateContextTokens(history, turn, ''), tokens.estimateContextTokens(refreshedHistory, [], ''));
});

test('large history uses the same bounded transcript window as the model context', () => {
  const history = Array.from({ length: 80 }, (_, index) => ({
    role: 'assistant', intent: 'result', body: `message-${index} ${'content '.repeat(220)}`,
  }));
  const compact = tokens.compactMessages(history, []);
  assert.ok(compact.length <= 50);
  const compactChars = compact
    .map(message => [message.role, message.intent, message.body].filter(Boolean).join('\n'))
    .join('\n').length;
  assert.ok(compactChars <= 24100);
  assert.ok(tokens.estimateContextTokens(history, [], '') <= 6200);
});
