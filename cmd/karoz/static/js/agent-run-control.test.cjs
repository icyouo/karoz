const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  agentRunCancelPath,
  claimAgentRunStop,
  agentRunControlPresentation,
  agentComposerEnterAction,
  stopActiveAgentRun,
} = require('./agents.js');

assert.deepEqual(agentRunControlPresentation(false, false), {
  text: 'Send ↗',
  title: 'Send message',
  ariaLabel: 'Send message to agent',
  danger: false,
  disabled: false,
});
assert.deepEqual(agentRunControlPresentation(true, false), {
  text: 'Stop',
  title: 'Stop active run',
  ariaLabel: 'Stop active agent run',
  danger: true,
  disabled: false,
});
assert.deepEqual(agentRunControlPresentation(true, true), {
  text: 'Stopping…',
  title: 'Stopping active run',
  ariaLabel: 'Stopping active agent run',
  danger: true,
  disabled: true,
});

assert.equal(agentRunCancelPath('project one', 'agent/two'), '/api/projects/project%20one/agents/agent%2Ftwo/run/cancel');
const stopping = {};
assert.equal(claimAgentRunStop(stopping, 'project', 'agent'), true);
assert.equal(claimAgentRunStop(stopping, 'project', 'agent'), false);
assert.equal(claimAgentRunStop(stopping, 'project', 'other'), true);

assert.equal(agentComposerEnterAction(false, false), 'send');
assert.equal(agentComposerEnterAction(true, false), 'interrupt');
assert.equal(agentComposerEnterAction(true, true), 'none');

const agentsSource = fs.readFileSync(path.join(__dirname, 'agents.js'), 'utf8');
const stopSource = agentsSource.slice(
  agentsSource.indexOf('async function stopActiveAgentRun()'),
  agentsSource.indexOf('function renderAgentWorkingState()'),
);
assert.match(stopSource, /claimAgentRunStop\(state\.agentRunStoppingByKey, projectID, agentID\)/);
assert.match(stopSource, /await api\(agentRunCancelPath\(projectID, agentID\), \{\s*method: 'POST',\s*body: JSON\.stringify\(\{\}\)/);
assert.doesNotMatch(stopSource, /agentMessage|\.value/);

const bindingsSource = fs.readFileSync(path.join(__dirname, 'bindings.js'), 'utf8');
assert.match(bindingsSource, /currentAgentWorking\(\) \|\| currentAgentStopping\(\)/);
assert.match(bindingsSource, /void stopActiveAgentRun\(\)/);
const initSource = fs.readFileSync(path.join(__dirname, 'init.js'), 'utf8');
assert.match(initSource, /agentComposerEnterAction\(currentAgentWorking\(\), currentAgentStopping\(\)\)/);
assert.match(initSource, /action === 'interrupt'\) void sendAgentMessage\(\)/);

async function testStopRequest() {
  const elements = {
    sendAgent: {
      textContent: '',
      title: '',
      disabled: false,
      setAttribute() {},
      classList: { toggle() {} },
    },
    agentWorkingPulse: { hidden: false },
    agentStatus: { textContent: '' },
    agentMessage: { value: 'keep this draft' },
  };
  global.$ = id => elements[id] || null;
  global.state = {
    project: { id: 'project one' },
    agent: { id: 'agent/two', nickname: 'Agent', state: 'working' },
    agents: [],
    providers: [],
    agentWorkingById: {},
    agentRunStoppingByKey: {},
  };
  const requests = [];
  const notifications = [];
  global.api = async (url, options) => {
    requests.push({ url, options });
    if (!options) throw new Error('test refresh unavailable');
    return { cancelled: true };
  };
  global.notify = message => notifications.push(message);

  await Promise.all([stopActiveAgentRun(), stopActiveAgentRun()]);
  assert.deepEqual(notifications, []);
  const mutations = requests.filter(request => request.options && request.options.method === 'POST');
  assert.equal(mutations.length, 1);
  assert.deepEqual(mutations[0], {
    url: '/api/projects/project%20one/agents/agent%2Ftwo/run/cancel',
    options: { method: 'POST', body: '{}' },
  });
  assert.equal(elements.agentMessage.value, 'keep this draft');

  state.agentRunStoppingByKey = {};
  notifications.length = 0;
  global.api = async (url, options) => {
    if (!options) throw new Error('test refresh unavailable');
    const error = new Error('agent has no active run');
    error.status = 409;
    throw error;
  };
  await stopActiveAgentRun();
  assert.deepEqual(notifications, []);
  assert.deepEqual(state.agentRunStoppingByKey, {});
}

testStopRequest()
  .then(() => console.log('agent run control: ok'))
  .catch(error => {
    console.error(error);
    process.exitCode = 1;
  });
