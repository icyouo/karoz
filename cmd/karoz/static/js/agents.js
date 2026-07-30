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
      requestActiveRunSync();
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
      // A scheduled Run can begin after the runtime snapshot was produced,
      // leaving the agent list temporarily idle. Probe the Run endpoint
      // independently so a refresh still attaches to its replay stream.
      if (state.agent && !state.chatStreaming && state.view === 'agent') requestActiveRunSync();
      // Backstop: SSE events can be missed or dropped; a visibly working
      // conversation also needs a persisted-history refresh.
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
      if (backgroundActivityRefreshTimer) {
        clearTimeout(backgroundActivityRefreshTimer);
        backgroundActivityRefreshTimer = null;
      }
      if (agentPollTimer) {
        clearInterval(agentPollTimer);
        agentPollTimer = null;
      }
      if (runtimeEvents) {
        runtimeEvents.close();
        runtimeEvents = null;
      }
      if (agentRunEvents) {
        agentRunEvents.close();
        agentRunEvents = null;
      }
      agentRunEventsKey = '';
      runtimeEventsProjectID = '';
      agentRunSyncQueued = false;
    }
    function requestActiveRunSync() {
      agentRunSyncQueued = true;
      if (agentRunSyncInFlight) return;
      agentRunSyncInFlight = true;
      void (async () => {
        try {
          while (agentRunSyncQueued) {
            agentRunSyncQueued = false;
            try { await syncActiveRunEvents(); } catch {}
          }
        } finally {
          agentRunSyncInFlight = false;
          // A runtime event can arrive while the final probe resolves.
          if (agentRunSyncQueued) requestActiveRunSync();
        }
      })();
    }
    async function syncActiveRunEvents() {
      if (!state.project || !state.agent || state.chatStreaming || !window.EventSource) return;
      const projectID = state.project.id;
      const agentID = currentAgentID();
      let status;
      try {
        status = await api('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID) + '/run');
      } catch {
        return;
      }
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      const run = status && status.active && status.run;
      if (!run || !run.id) {
        setLocalAgentWorking(agentID, false);
        if (state.agent && state.agent.id === agentID) {
          if (state.agent.state === 'working') state.agent.state = 'idle';
          if (state.agent.status_message === 'working') state.agent.status_message = 'ready';
        }
        if (agentRunEventsKey.startsWith(projectID + ':' + agentID + ':')) {
          agentRunEvents?.close();
          agentRunEvents = null;
          agentRunEventsKey = '';
        }
        if (state.activeRunAgentID === agentID) state.activeRunID = '';
        renderAgentWorkingState();
        return;
      }
      setLocalAgentWorking(agentID, true);
      renderAgentWorkingState();
      const key = projectID + ':' + agentID + ':' + run.id;
      if (agentRunEventsKey === key && agentRunEvents) return;
      if (agentRunEvents) agentRunEvents.close();
      if (state.activeRunID !== run.id || state.activeRunAgentID !== agentID) {
        state.activeRunID = run.id;
        state.activeRunAgentID = agentID;
        state.lastRunSeq = 0;
        clearActiveRunReplay();
      }
      agentRunEventsKey = key;
      agentRunEvents = new EventSource('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID) + '/runs/' + encodeURIComponent(run.id) + '/events?after=' + encodeURIComponent(state.lastRunSeq));
      const replay = event => {
        try {
          dispatchAgentSSE('event: ' + event.type + '\\ndata: ' + event.data, {
            onDelta: appendActiveRunReplayDelta,
            onToolStart: appendActiveRunReplayToolStart,
            onToolResult: appendActiveRunReplayToolResult,
            onPreview: scheduleChatRefresh,
            onInterrupt: scheduleChatRefresh,
            onReset: () => { clearActiveRunReplay(run.id); clearCurrentContextTurn(); },
            onDone: async () => { agentRunEvents?.close(); agentRunEvents = null; agentRunEventsKey = ''; await refreshActiveAgentChat(); clearActiveRunReplay(run.id); clearCurrentContextTurn(); await refreshAgentStates(); },
            onCancelled: async () => { agentRunEvents?.close(); agentRunEvents = null; agentRunEventsKey = ''; await refreshActiveAgentChat(); clearActiveRunReplay(run.id); clearCurrentContextTurn(); await refreshAgentStates(); },
            onError: async () => { agentRunEvents?.close(); agentRunEvents = null; agentRunEventsKey = ''; await refreshActiveAgentChat(); clearActiveRunReplay(run.id); clearCurrentContextTurn(); await refreshAgentStates(); },
          });
        } catch {}
      };
      ['meta', 'delta', 'tool_start', 'tool_result', 'preview', 'interrupt', 'done', 'cancelled', 'error', 'reset'].forEach(type => agentRunEvents.addEventListener(type, replay));
      agentRunEvents.onerror = () => {
        if (agentRunEventsKey !== key) return;
        agentRunEvents?.close();
        agentRunEvents = null;
        setTimeout(() => {
          if (state.project && state.project.id === projectID && currentAgentID() === agentID && agentRunEventsKey === key) {
            agentRunEventsKey = '';
            requestActiveRunSync();
          }
        }, 1000);
      };
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
        try {
          applyPayload(JSON.parse(event.data));
          // Do not gate this on agentWorking(): scheduled Runs may not yet
          // be reflected in the agents snapshot.
          requestActiveRunSync();
          scheduleChatRefresh();
          scheduleBackgroundActivityRefresh();
        } catch {}
      });
      runtimeEvents.addEventListener('runtime', event => {
		try {
		  const payload = JSON.parse(event.data);
		  applyPayload(payload);
		  // Runtime events are the prompt notification for Runs that start
		  // after the page loaded, including scheduled jobs with stale state.
		  requestActiveRunSync();
		  if (payload && payload.event) maybeAnimateHandoff(payload.event);
		  clearTimeout(runtimeStateRefreshTimer);
		  runtimeStateRefreshTimer = setTimeout(() => loadResidentRuntimeState(), 120);
		  scheduleBackgroundActivityRefresh();
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
      if (!state.project) {
        renderAgentWorkingState();
        return;
      }
      state.agents.forEach(agent => {
        const b = document.createElement('button');
        b.className = 'nav-item' + (state.agent && state.agent.id === agent.id ? ' active' : '');
        b.dataset.agentId = agent.id;
        const label = agent.nickname || agent.display_name || agent.name || 'agent';
        const group = agent.group_id ? '<span class="group-tag">' + escapeHTML(agent.group_id) + (agent.group_role ? ' · ' + escapeHTML(agent.group_role) : '') + '</span>' : '';
        const working = agentWorking(agent) ? '<span class="agent-working-pulse" aria-label="Agent is working"><i></i><i></i><i></i></span>' : '';
        b.innerHTML = '<strong>' + escapeHTML(label) + working + '</strong><div class="muted">' + escapeHTML(agent.short_name || agent.name || 'agent') + ' · ' + escapeHTML(agent.runtime || 'resident') + ' · ' + (agent.message_count || 0) + ' msgs</div>' + group;
        b.onclick = () => selectAgent(agent, { push: true });
        box.appendChild(b);
      });
      renderAgentWorkingState();
    }
    // Handoff animation: a dot travels from the source agent's row to the
    // target's in the sidebar, and both rows pulse. Deduped per handoff
    // because creation fires both handoff_created and handoff_changed.
    const recentHandoffAnimations = new Map();
    function maybeAnimateHandoff(event) {
      if (!event || !event.from_agent_id || !event.to_agent_id) return;
      if (event.kind !== 'handoff_created' && event.kind !== 'handoff_changed') return;
      const key = (event.entity_id || '') + '|' + event.from_agent_id + '|' + event.to_agent_id;
      const now = Date.now();
      if (recentHandoffAnimations.has(key) && now - recentHandoffAnimations.get(key) < 4000) return;
      recentHandoffAnimations.set(key, now);
      setTimeout(() => recentHandoffAnimations.delete(key), 8000);
      animateHandoff(event.from_agent_id, event.to_agent_id);
    }
    function animateHandoff(fromAgentID, toAgentID) {
      const list = $('agentList');
      if (!list || state.view !== 'agent') return;
      const fromEl = list.querySelector('[data-agent-id="' + CSS.escape(fromAgentID) + '"]');
      const toEl = list.querySelector('[data-agent-id="' + CSS.escape(toAgentID) + '"]');
      if (!fromEl || !toEl) return;
      [fromEl, toEl].forEach(el => {
        el.classList.remove('handoff-pulse');
        void el.offsetWidth;
        el.classList.add('handoff-pulse');
        setTimeout(() => el.classList.remove('handoff-pulse'), 1400);
      });
      const fromRect = fromEl.getBoundingClientRect();
      const toRect = toEl.getBoundingClientRect();
      const dot = document.createElement('div');
      dot.className = 'handoff-dot';
      dot.style.left = (fromRect.right - 18) + 'px';
      dot.style.top = (fromRect.top + fromRect.height / 2 - 4) + 'px';
      document.body.appendChild(dot);
      requestAnimationFrame(() => {
        dot.style.transform = 'translate(' + (toRect.right - fromRect.right) + 'px, ' + ((toRect.top + toRect.height / 2) - (fromRect.top + fromRect.height / 2)) + 'px)';
        dot.style.opacity = '0';
      });
      setTimeout(() => dot.remove(), 1100);
    }
    async function selectAgent(agent, opts = {}) {
      if (!state.project || !agent) return;
      const projectID = state.project.id;
      const agentID = agent.id;
      if (state.activeRunAgentID !== agentID) {
        if (agentRunEvents) agentRunEvents.close();
        agentRunEvents = null;
        agentRunEventsKey = '';
        state.activeRunID = '';
        state.activeRunAgentID = agentID;
        state.lastRunSeq = 0;
        clearActiveRunReplay();
      }
      state.agent = agent;
      renderAgents();
      updateAgentChrome();
      await loadAgentMessages();
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      await loadResidentRuntimeState();
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      switchView('agent');
      requestActiveRunSync();
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
      renderContextTokenUsage();
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
      renderContextTokenUsage();
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
    function agentRunControlKey(projectID, agentID) {
      return String(projectID || '') + ':' + String(agentID || '');
    }
    function agentRunCancelPath(projectID, agentID) {
      return '/api/projects/' + encodeURIComponent(projectID) + '/agents/' + encodeURIComponent(agentID) + '/run/cancel';
    }
    function claimAgentRunStop(stoppingByKey, projectID, agentID) {
      const key = agentRunControlKey(projectID, agentID);
      if (stoppingByKey[key]) return false;
      stoppingByKey[key] = true;
      return true;
    }
    function currentAgentStopping() {
      if (!state.project || !state.agent) return false;
      return !!state.agentRunStoppingByKey[agentRunControlKey(state.project.id, state.agent.id)];
    }
    function setAgentRunStopping(projectID, agentID, stopping) {
      const key = agentRunControlKey(projectID, agentID);
      if (stopping) state.agentRunStoppingByKey[key] = true;
      else delete state.agentRunStoppingByKey[key];
    }
    function agentRunControlPresentation(working, stopping) {
      if (stopping) {
        return {
          text: 'Stopping…',
          title: 'Stopping active run',
          ariaLabel: 'Stopping active agent run',
          danger: true,
          disabled: true,
        };
      }
      if (working) {
        return {
          text: 'Stop',
          title: 'Stop active run',
          ariaLabel: 'Stop active agent run',
          danger: true,
          disabled: false,
        };
      }
      return {
        text: 'Send ↗',
        title: 'Send message',
        ariaLabel: 'Send message to agent',
        danger: false,
        disabled: false,
      };
    }
    function agentComposerEnterAction(working, stopping) {
      if (stopping) return 'none';
      return working ? 'interrupt' : 'send';
    }
    async function stopActiveAgentRun() {
      if (!state.project || !state.agent) return;
      const projectID = state.project.id;
      const agentID = state.agent.id;
      if (!claimAgentRunStop(state.agentRunStoppingByKey, projectID, agentID)) return;
      renderAgentWorkingState();
      try {
        await api(agentRunCancelPath(projectID, agentID), {
          method: 'POST',
          body: JSON.stringify({}),
        });
        if (state.project && state.project.id === projectID &&
            state.agent && state.agent.id === agentID) {
          $('agentStatus').textContent = currentAgentLabel() + ' · cancelled · refreshing';
        }
        await refreshAgentStates();
      } catch (err) {
        setAgentRunStopping(projectID, agentID, false);
        if (err && err.status === 409) {
          await refreshAgentStates();
          renderAgentWorkingState();
          return;
        }
        renderAgentWorkingState();
        notify('Could not stop agent run: ' + ((err && err.message) || String(err)), 'error');
      }
    }
    function renderAgentWorkingState() {
      const pulse = $('agentWorkingPulse');
      const send = $('sendAgent');
      const working = currentAgentWorking();
      let stopping = currentAgentStopping();
      if (stopping && !working && state.project && state.agent) {
        setAgentRunStopping(state.project.id, state.agent.id, false);
        stopping = false;
      }
      if (pulse) pulse.hidden = !(working || stopping);
      if (send) {
        const presentation = agentRunControlPresentation(working, stopping);
        send.textContent = presentation.text;
        send.title = presentation.title;
        send.setAttribute('aria-label', presentation.ariaLabel);
        send.classList.toggle('danger', presentation.danger);
        send.disabled = presentation.disabled;
      }
      restoreAgentModelSettings();
    }
    if (typeof module !== 'undefined' && module.exports) {
      module.exports = {
        agentRunCancelPath,
        claimAgentRunStop,
        agentRunControlPresentation,
        agentComposerEnterAction,
        stopActiveAgentRun,
        syncActiveRunEvents,
      };
    }
