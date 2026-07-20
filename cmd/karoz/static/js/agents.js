    async function loadAgents() {
      if (!state.project) return;
      const projectID = state.project.id;
      const agents = await api('/api/projects/' + projectID + '/agents') || [];
      if (!state.project || state.project.id !== projectID) return;
      state.agents = agents;
      const previousId = state.agent && state.agent.id;
      state.agent = agents.find(agent => agent.id === previousId) || agents[0] || null;
      renderAgents();
      await loadAgentMessages();
      if (!state.project || state.project.id !== projectID) return;
      await loadResidentRuntimeState();
    }
    async function refreshAgentStates() {
      if (!state.project) return;
      const agents = await api('/api/projects/' + state.project.id + '/agents').catch(() => null);
      if (!agents) return;
      const previousId = state.agent && state.agent.id;
      state.agents = agents;
      state.agent = agents.find(agent => agent.id === previousId) || state.agent || agents[0] || null;
      renderAgents();
      renderRuntimeStrip();
      // Backstop: SSE events can be missed or dropped; while the open
      // conversation's agent is working, keep its messages syncing.
      if (state.agent && agentWorking(state.agent) && !state.chatStreaming && state.view === 'agent') scheduleChatRefresh();
    }
    function syncAgentPolling() {
      if (agentPollTimer) {
        clearInterval(agentPollTimer);
        agentPollTimer = null;
      }
      if (!state.project) return;
      agentPollTimer = setInterval(refreshAgentStates, 10000);
    }
    function stopRuntimeSubscriptions() {
	  if (runtimeStateRefreshTimer) {
		clearTimeout(runtimeStateRefreshTimer);
		runtimeStateRefreshTimer = null;
	  }
      if (agentPollTimer) {
        clearInterval(agentPollTimer);
        agentPollTimer = null;
      }
      if (runtimeEvents) {
        runtimeEvents.close();
        runtimeEvents = null;
      }
      runtimeEventsProjectID = '';
    }
    function syncRuntimeEvents() {
      if (runtimeEvents) {
        runtimeEvents.close();
        runtimeEvents = null;
      }
      runtimeEventsProjectID = state.project ? state.project.id : '';
      if (!state.project || !window.EventSource) return;
      const projectID = state.project.id;
      runtimeEvents = new EventSource('/api/projects/' + projectID + '/runtime-events');
      const applyPayload = (payload) => {
        if (!state.project || state.project.id !== projectID) return;
        if (payload && Array.isArray(payload.agents)) {
          const previousId = state.agent && state.agent.id;
          state.agents = payload.agents;
          state.agent = state.agents.find(agent => agent.id === previousId) || state.agent || state.agents[0] || null;
          renderAgents();
          renderRuntimeStrip();
        } else {
          refreshAgentStates();
        }
      };
      runtimeEvents.addEventListener('snapshot', event => {
        try { applyPayload(JSON.parse(event.data)); scheduleChatRefresh(); } catch {}
      });
      runtimeEvents.addEventListener('runtime', event => {
		try {
		  applyPayload(JSON.parse(event.data));
		  clearTimeout(runtimeStateRefreshTimer);
		  runtimeStateRefreshTimer = setTimeout(() => loadResidentRuntimeState(), 120);
		  scheduleChatRefresh();
		} catch {}
      });
      runtimeEvents.onerror = () => {
        if (runtimeEvents && runtimeEventsProjectID === projectID) {
          runtimeEvents.close();
          runtimeEvents = null;
        }
        setTimeout(() => {
          if (state.project && state.project.id === projectID && runtimeEventsProjectID === projectID && !runtimeEvents) syncRuntimeEvents();
        }, 1500);
      };
    }
    async function loadProjectSkills() {
      if (!state.project) return [];
      if (state.skillsProjectID === state.project.id && Array.isArray(state.skills)) return state.skills;
      state.skills = await api('/api/projects/' + state.project.id + '/skills').catch(() => []);
      state.skillsProjectID = state.project.id;
      return state.skills;
    }
    function renderAgents() {
      const box = $('agentList'); box.innerHTML = '';
      if (!state.project) return;
      state.agents.forEach(agent => {
        const b = document.createElement('button');
        b.className = 'nav-item' + (state.agent && state.agent.id === agent.id ? ' active' : '');
        const label = agent.nickname || agent.display_name || agent.name || 'agent';
        const group = agent.group_id ? '<span class="group-tag">' + escapeHTML(agent.group_id) + (agent.group_role ? ' · ' + escapeHTML(agent.group_role) : '') + '</span>' : '';
        const working = agentWorking(agent) ? '<span class="agent-working-pulse" aria-label="Agent is working"><i></i><i></i><i></i></span>' : '';
        b.innerHTML = '<strong>' + escapeHTML(label) + working + '</strong><div class="muted">' + escapeHTML(agent.short_name || agent.name || 'agent') + ' · ' + escapeHTML(agent.runtime || 'resident') + ' · ' + (agent.message_count || 0) + ' msgs</div>' + group;
        b.onclick = () => selectAgent(agent, { push: true });
        box.appendChild(b);
      });
    }
    async function selectAgent(agent, opts = {}) {
      if (!state.project || !agent) return;
      const projectID = state.project.id;
      const agentID = agent.id;
      state.agent = agent;
      renderAgents();
      updateAgentChrome();
      await loadAgentMessages();
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      await loadResidentRuntimeState();
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      switchView('agent');
      syncRouteHash(Boolean(opts.push));
    }
    function currentAgentID() {
      return state.agent && state.agent.id ? state.agent.id : 'karoz';
    }
    function currentAgentLabel() {
      const agent = state.agent || {};
      return agent.nickname || agent.display_name || agent.name || 'Karoz';
    }
    function shortAgentRole() {
      const agent = state.agent || {};
      const raw = agent.short_name || agent.group_role || agent.name || agent.runtime || 'resident';
      const text = String(raw || 'resident').trim();
      if (!text) return 'resident';
      const normalized = text.toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
      const aliases = {
        'shape technical architecture and integration contracts': 'architect',
        'technical architecture and integration contracts': 'architect',
        'implementation lead': 'impl',
        'frontend specialist': 'frontend',
        'product strategist': 'strategy',
        'research scan': 'research',
        'review critic': 'review'
      };
      if (aliases[normalized]) return aliases[normalized];
      const parts = normalized.split(/\s+/).filter(Boolean);
      if (parts.length > 3) return parts.slice(0, 2).join('-');
      return normalized || text.slice(0, 18);
    }
    function titleCaseCompact(text) {
      return String(text || '')
        .split(/[-\s]+/)
        .filter(Boolean)
        .map(part => part.slice(0, 1).toUpperCase() + part.slice(1))
        .join(' ');
    }
    function normalizeAgentChatType(value) {
      const mode = String(value || '').trim().toLowerCase();
      return ['ask', 'plan', 'dev'].includes(mode) ? mode : 'ask';
    }
    function renderAgentChatType() {
      document.querySelectorAll('.chat-mode').forEach(button => {
        const active = button.dataset.chatType === state.chatType;
        button.classList.toggle('active', active);
        button.setAttribute('aria-selected', String(active));
      });
    }
    function restoreAgentChatType() {
      state.chatType = normalizeAgentChatType(state.agent && state.agent.chat_mode);
      renderAgentChatType();
    }
    function restoreAgentModelSettings() {
      const modelSelect = $('agentModel');
      const effortSelect = $('agentThinkingEffort');
      if (!modelSelect || !effortSelect) return;
      const model = String((state.agent && state.agent.model) || 'gpt-5.6-luna').trim();
      if (!Array.from(modelSelect.options).some(option => option.value === model)) {
        const option = document.createElement('option');
        option.value = model;
        option.textContent = model;
        modelSelect.prepend(option);
      }
      modelSelect.value = model;
      syncEffortOptionsForSelectedModel(String((state.agent && state.agent.thinking_effort) || 'medium').toLowerCase());
      modelSelect.disabled = currentAgentWorking();
    }
    function syncEffortOptionsForSelectedModel(preferred = 'medium') {
      const effortSelect = $('agentThinkingEffort');
      const selected = selectedModelDescriptor();
      const levels = selected && Array.isArray(selected.model.effort_levels) ? selected.model.effort_levels : [];
      effortSelect.innerHTML = '';
      if (!levels.length) {
        const option = document.createElement('option'); option.value = ''; option.textContent = 'Default'; effortSelect.appendChild(option);
        effortSelect.disabled = true;
      } else {
        levels.forEach(level => { const option = document.createElement('option'); option.value = level; option.textContent = level.slice(0, 1).toUpperCase() + level.slice(1); effortSelect.appendChild(option); });
        effortSelect.disabled = currentAgentWorking();
      }
      const saved = String(preferred || (levels.includes('medium') ? 'medium' : levels[0] || '')).toLowerCase();
      effortSelect.value = levels.includes(saved) ? saved : (levels.includes('medium') ? 'medium' : levels[0] || '');
    }
    async function saveAgentModelSettings() {
      if (!state.project || !state.agent) return;
      const projectID = state.project.id;
      const agentID = state.agent.id;
      const model = $('agentModel').value;
      const thinkingEffort = $('agentThinkingEffort').value;
      const selected = selectedModelDescriptor();
      if (!selected || !selected.provider.available) return restoreAgentModelSettings();
      const provider = selected.provider.id;
      const serial = ++modelSettingsSaveSerial;
      $('agentModel').disabled = true;
      $('agentThinkingEffort').disabled = true;
      try {
        const updated = await api('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID), { method: 'PATCH', body: JSON.stringify({ provider, model, thinking_effort: thinkingEffort, expected_model_config_version: state.agent.model_config_version || 1 }) });
        if (state.project && state.project.id === projectID) {
          const index = state.agents.findIndex(agent => agent.id === agentID);
          if (index >= 0) state.agents[index] = updated;
          if (state.agent && state.agent.id === agentID) state.agent = updated;
          restoreAgentModelSettings();
        }
      } catch (err) {
        restoreAgentModelSettings();
        notify('Could not save model settings: ' + (err.message || String(err)), 'error');
      } finally {
        if (serial === modelSettingsSaveSerial) {
          restoreAgentModelSettings();
        }
      }
    }
    async function saveAgentChatType(value) {
      if (!state.project || !state.agent) return;
      const mode = normalizeAgentChatType(value);
      const projectID = state.project.id;
      const agentID = state.agent.id;
      const previous = normalizeAgentChatType(state.agent.chat_mode);
      const serial = ++chatModeSaveSerial;
      state.chatType = mode;
      state.agent.chat_mode = mode;
      const localAgent = state.agents.find(agent => agent.id === agentID);
      if (localAgent) localAgent.chat_mode = mode;
      renderAgentChatType();
      document.querySelectorAll('.chat-mode').forEach(button => { button.disabled = true; });
      try {
        const updated = await api('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID), { method: 'PATCH', body: JSON.stringify({ chat_mode: mode }) });
        if (state.project && state.project.id === projectID) {
          const index = state.agents.findIndex(agent => agent.id === agentID);
          if (index >= 0) state.agents[index] = updated;
          if (state.agent && state.agent.id === agentID && state.chatType === mode) {
            state.agent = updated;
            state.chatType = normalizeAgentChatType(updated.chat_mode);
            renderAgentChatType();
          }
        }
      } catch (err) {
        if (state.project && state.project.id === projectID) {
          const item = state.agents.find(agent => agent.id === agentID);
          if (item) item.chat_mode = previous;
          if (state.agent && state.agent.id === agentID && state.chatType === mode) {
            state.agent.chat_mode = previous;
            state.chatType = previous;
            renderAgentChatType();
          }
        }
        notify('Could not save agent mode: ' + (err.message || String(err)), 'error');
      } finally {
        if (serial === chatModeSaveSerial) document.querySelectorAll('.chat-mode').forEach(button => { button.disabled = false; });
      }
    }
    function updateAgentChrome() {
      const label = currentAgentLabel();
      restoreAgentChatType();
      restoreAgentModelSettings();
      $('agentMessage').placeholder = 'Message ' + label + '...';
      $('agentStatus').textContent = label + ' resident session';
      $('agentHeaderName').textContent = label;
      renderAgentWorkingState();
      if (state.project) $('projectMeta').textContent = projectWorkspaceLabel(state.project) + ' · branch ' + state.project.default_branch + ' · agent ' + label;
      renderRuntimeStrip();
    }
    function currentAgentWorking() {
      return agentWorking(state.agent);
    }
    function agentWorking(agent) {
      if (!agent || !agent.id) return false;
      return !!state.agentWorkingById[agent.id] || agent.state === 'working' || agent.status_message === 'working';
    }
    function setLocalAgentWorking(agentId, working) {
      if (!agentId) return;
      if (working) state.agentWorkingById[agentId] = true;
      else delete state.agentWorkingById[agentId];
    }
    function renderAgentWorkingState() {
      const pulse = $('agentWorkingPulse');
      if (!pulse) return;
      pulse.hidden = !currentAgentWorking();
      restoreAgentModelSettings();
    }
