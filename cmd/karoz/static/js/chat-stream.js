    async function sendAgentMessage(messageOverride = '', choiceID = '', attachmentOverride = null) {
      if (!state.project || !state.agent) return notify('Select a project and agent first.', 'error');
      const directChoice = Boolean(choiceID);
      const message = directChoice ? String(messageOverride || '').trim() : $('agentMessage').value.trim();
      const attachments = Array.isArray(attachmentOverride) ? attachmentOverride.slice() : (state.agentAttachments || []).slice();
      if (!message && attachments.length === 0) return fieldError('agentMessage', 'Enter a message or attach a file.');
      const activeAgentId = currentAgentID();
      const wasWorking = currentAgentWorking();
      const contextMessage = messagePreviewWithAttachments(message, attachments);
      $('sendAgent').disabled = true;
      appendAgentMessage('you', contextMessage);
      if (wasWorking) appendCurrentContextEvent('user', 'interrupt', contextMessage);
      else beginCurrentContextTurn(contextMessage);
      if (!directChoice) {
        $('agentMessage').value = '';
        clearAgentAttachments();
      }
      closeSkillSuggest();
      if (wasWorking) {
        $('agentStatus').textContent = currentAgentLabel() + ' · queued interrupt ·';
        try {
          await consumeAgentStream(message, attachments, {
            onQueued(payload) {
              $('agentStatus').textContent = currentAgentLabel() + ' · working · interrupt queued';
            },
            onDone(payload) {
              $('agentStatus').textContent = currentAgentLabel() + ' · working · interrupt pending';
            },
            onError(message) {
              throw new Error(message);
            }
          }, choiceID);
        } catch (err) {
          state.agentAttachments = attachments;
          renderAgentAttachments();
          appendAgentMessage(currentAgentID(), 'Queue failed: ' + err.message);
          $('agentStatus').textContent = currentAgentLabel() + ' queue failed: ' + err.message;
        } finally {
          await refreshActiveAgentChat();
          if (!state.chatStreaming) clearCurrentContextTurn();
          await refreshAgentStates();
          $('sendAgent').disabled = false;
          $('agentMessage').focus();
        }
        return;
      }
      setLocalAgentWorking(activeAgentId, true);
      state.activeRunID = '';
      state.activeRunAgentID = activeAgentId;
      state.lastRunSeq = 0;
      clearActiveRunReplay();
      state.chatStreaming = true;
      if (state.agent) {
        state.agent.state = 'working';
        state.agent.status_message = 'working';
      }
      renderAgents();
      renderRuntimeStrip();
      $('agentStatus').textContent = currentAgentLabel() + ' · working ·';
      const assistantItem = appendAgentMessage(currentAgentID(), '');
      const assistantBubble = assistantItem.querySelector('.chat-bubble');
      let assistantText = '';
      let renderedChoice = false;
      try {
        await consumeAgentStream(message, attachments, {
          onMeta(payload) {
            if (payload && payload.agent && state.agent && payload.agent.id === state.agent.id) {
              state.agent = payload.agent;
              renderRuntimeStrip();
            }
          },
          onDelta(delta) {
            assistantText += delta;
            updateCurrentContextAssistant(assistantText);
            setBubbleContent(assistantBubble, assistantText, true);
            $('agentOutput').scrollTop = $('agentOutput').scrollHeight;
          },
          onDone(payload) {
            if (payload && payload.agent && state.agent && payload.agent.id === state.agent.id) {
              state.agent = payload.agent;
            }
            assistantText = payload.message || assistantText;
            updateCurrentContextAssistant(assistantText);
            if (renderedChoice) {
              assistantItem.remove();
            } else {
              setBubbleContent(assistantBubble, assistantText || '(empty response)', true);
            }
            $('agentStatus').textContent = currentAgentLabel() + ' · ready';
          },
          onError(message) {
            throw new Error(message);
          },
          onCancelled(payload) {
            assistantText = (payload && payload.message) || 'Agent run cancelled.';
            updateCurrentContextAssistant(assistantText);
            setBubbleContent(assistantBubble, assistantText, false);
            $('agentStatus').textContent = currentAgentLabel() + ' · cancelled';
          },
          onToolStart(payload) {
            appendCurrentContextEvent('tool_call', payload.tool || 'tool', payload.arguments);
            if (payload.tool !== 'request_choice') appendToolMessage('tool_call', payload.tool, payload.call_id, payload.arguments, true);
          },
          onToolResult(payload) {
            appendCurrentContextEvent('tool_result', payload.tool || 'tool', payload.result);
            if (appendChoiceRequestFromResult(payload.result, true)) {
              renderedChoice = true;
              return;
            }
            appendToolMessage('tool_result', payload.tool, payload.call_id, payload.result, payload.success);
          },
          onPreview(payload) {
            if (!payload) return;
            state.preview = {
              filename: payload.filename || payload.path || 'preview.html',
              path: payload.path || payload.filename || 'preview.html',
              mime_type: payload.mimeType || payload.mime_type || 'text/html',
			  encoding: payload.encoding || 'utf-8',
              content: payload.content || ''
            };
            state.sidePanel = 'preview';
            renderSidePane();
          },
          onInterrupt(payload) {
            const count = payload && Array.isArray(payload.interrupts) ? payload.interrupts.length : 0;
            $('agentStatus').textContent = currentAgentLabel() + ' · working · applied ' + (count || 1) + ' interrupt';
          }
        }, choiceID);
        if (renderedChoice) {
          await refreshAgentStates();
        } else {
          await loadAgents();
        }
        await loadTasks();
        await loadResidentRuntimeState();
        await loadWorkspaceFiles();
        const latestHTML = state.workspaceFiles.find(file => /\.html?$/i.test(file.filename));
        if (latestHTML) await openWorkspaceFile(latestHTML.path);
      } catch (err) {
        state.agentAttachments = attachments;
        renderAgentAttachments();
        setBubbleContent(assistantBubble, 'Request failed: ' + err.message, false);
        updateCurrentContextAssistant('Request failed: ' + err.message);
        $('agentStatus').textContent = currentAgentLabel() + ' request failed: ' + err.message;
      } finally {
        state.chatStreaming = false;
        setLocalAgentWorking(activeAgentId, false);
        await refreshActiveAgentChat();
        clearCurrentContextTurn();
        await refreshAgentStates();
        renderRuntimeStrip();
        $('sendAgent').disabled = false;
        $('agentMessage').focus();
      }
    }
    async function consumeAgentStream(message, attachments, handlers, choiceID = '') {
      const hasAttachments = Array.isArray(attachments) && attachments.length > 0;
      let body;
      const headers = { 'accept': 'text/event-stream' };
      if (hasAttachments) {
        body = new FormData();
        body.append('message', message);
        body.append('type', state.chatType || 'ask');
        if (choiceID) body.append('choice_id', choiceID);
        attachments.forEach(file => body.append('files', file, file.name));
      } else {
        headers['content-type'] = 'application/json';
        body = JSON.stringify({ message, type: state.chatType || 'ask', choice_id: choiceID });
      }
      const res = await fetch('/api/projects/' + state.project.id + '/agents/' + encodeURIComponent(currentAgentID()) + '/messages', {
        method: 'POST',
        headers,
        body
      });
      if (!res.ok) throw new Error(await res.text());
      if (!res.body) throw new Error('stream unavailable');
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let index;
        while ((index = buffer.indexOf('\n\n')) >= 0) {
          const raw = buffer.slice(0, index);
          buffer = buffer.slice(index + 2);
          dispatchAgentSSE(raw, handlers);
        }
      }
      if (buffer.trim()) dispatchAgentSSE(buffer, handlers);
    }
    function dispatchAgentSSE(raw, handlers) {
      let event = 'message';
      const data = [];
      raw.split('\n').forEach(line => {
        if (line.startsWith('event:')) event = line.slice(6).trim();
        if (line.startsWith('data:')) data.push(line.slice(5).trim());
      });
      if (!data.length) return;
      let payload = JSON.parse(data.join('\n'));
      if (payload && payload.run_id && Object.prototype.hasOwnProperty.call(payload, 'data')) {
        const envelope = payload;
        if (!KarozRunReplay.accept(state, envelope)) return;
        const dataPayload = envelope.data;
        payload = dataPayload && typeof dataPayload === 'object' && !Array.isArray(dataPayload)
          ? { ...dataPayload }
          : { value: dataPayload };
        payload._runID = envelope.run_id;
        payload._runSeq = Number(envelope.seq || 0);
      }
      if (event === 'meta') handlers.onMeta?.(payload);
      if (event === 'delta') handlers.onDelta?.(payload.delta || payload.content || '');
      if (event === 'tool_start') handlers.onToolStart?.(payload);
      if (event === 'tool_result') handlers.onToolResult?.(payload);
      if (event === 'preview') handlers.onPreview?.(payload);
      if (event === 'queued') handlers.onQueued?.(payload);
      if (event === 'interrupt') handlers.onInterrupt?.(payload);
      if (event === 'log') handlers.onLog?.(payload);
      if (event === 'reset') {
        KarozRunReplay.reset(state, payload.floor);
        handlers.onReset?.(payload);
        refreshActiveAgentChat();
      }
      if (event === 'done') handlers.onDone?.(payload);
      if (event === 'error') handlers.onError?.(payload.message || 'stream failed');
      if (event === 'cancelled') handlers.onCancelled?.(payload);
    }
    // Provider deltas are intentionally not persisted until the final result.
    // Keep one ephemeral, run-keyed assistant bubble so reconnect/replay after
    // a browser refresh remains visible. Tool events carry the sequence of
    // their already-persisted message, letting this renderer bridge a history
    // fetch race without duplicating durable cards.
    function ensureActiveRunReplay() {
      if (!state.activeRunID) return null;
      if (!state.activeRunReplay || state.activeRunReplay.runID !== state.activeRunID) {
        state.activeRunReplay = { runID: state.activeRunID, agentID: currentAgentID(), text: '', toolEvents: [] };
      }
      return state.activeRunReplay;
    }
    function appendActiveRunReplayDelta(delta) {
      if (!delta) return;
      const replay = ensureActiveRunReplay();
      if (!replay) return;
      replay.text += String(delta);
      rehydrateCurrentContextAssistant(replay.text);
      renderActiveRunReplay();
    }
    function removeActiveRunReplayElements() {
      const output = $('agentOutput');
      if (!output) return;
      output.querySelectorAll('.run-replay-message, .run-replay-event').forEach(item => item.remove());
      output.querySelectorAll('.tool-batch').forEach(batch => {
        if (!batch.querySelector('.tool-group')) {
          batch.remove();
        } else {
          updateToolBatchSummary(batch);
        }
      });
    }
    function renderActiveRunReplay() {
      const output = $('agentOutput');
      if (!output) return;
      removeActiveRunReplayElements();
      const replay = state.activeRunReplay;
      if (!replay || replay.agentID !== currentAgentID()) return;
      KarozRunReplay.transientToolEvents(replay, state.chatMessages).forEach(event => {
        const payload = event.payload || {};
        if (event.kind === 'tool_start') {
          if (payload.tool === 'request_choice') return;
          const item = appendToolMessage('tool_call', payload.tool, payload.call_id, payload.arguments, true);
          item.classList.add('run-replay-event');
          item.dataset.runId = replay.runID;
          return;
        }
        if (event.kind === 'tool_result') {
          const choice = parseChoiceRequestResult(payload.result);
          if (choice) {
            const item = appendChoiceRequest(choice, true);
            item.classList.add('run-replay-event');
            item.dataset.runId = replay.runID;
            return;
          }
          const item = appendToolMessage('tool_result', payload.tool, payload.call_id, payload.result, payload.success);
          item.classList.add('run-replay-event');
          item.dataset.runId = replay.runID;
        }
      });
      if (!replay.text) return;
      const item = appendAgentMessage(currentAgentID(), replay.text);
      item.classList.add('run-replay-message');
      item.dataset.runId = replay.runID;
    }
    function clearActiveRunReplay(runID = '') {
      if (runID && state.activeRunReplay && state.activeRunReplay.runID !== runID) return;
      state.activeRunReplay = null;
      removeActiveRunReplayElements();
    }
    function appendActiveRunReplayToolStart(payload) {
      const replay = ensureActiveRunReplay();
      if (!replay) return;
      if (KarozRunReplay.addToolEvent(replay, {
        kind: 'tool_start', seq: payload._runSeq, callID: payload.call_id,
        messageSeq: payload.message_seq, payload,
      })) {
        renderActiveRunReplay();
        scheduleChatRefresh();
      }
    }
    function appendActiveRunReplayToolResult(payload) {
      const replay = ensureActiveRunReplay();
      if (!replay) return;
      if (KarozRunReplay.addToolEvent(replay, {
        kind: 'tool_result', seq: payload._runSeq, callID: payload.call_id,
        messageSeq: payload.message_seq, payload,
      })) {
        renderActiveRunReplay();
        scheduleChatRefresh();
      }
    }
    function currentSkillTrigger(input) {
      const cursor = input.selectionStart || 0;
      const before = input.value.slice(0, cursor);
      const match = before.match(/(^|\s)([$/])([A-Za-z0-9_.:-]*)$/);
      if (!match) return null;
      const token = match[2] + match[3];
      const start = cursor - token.length;
      return { start, end: cursor, prefix: match[2], query: match[3] || '' };
    }
    async function updateSkillSuggest() {
      const input = $('agentMessage');
      const trigger = currentSkillTrigger(input);
      if (!trigger || !state.project) {
        closeSkillSuggest();
        return;
      }
      const query = trigger.query.toLowerCase();
      const skills = await loadProjectSkills();
      const items = skills
        .filter(skill => {
          const name = String(skill.name || '').toLowerCase();
          return !query || name.startsWith(query);
        })
        .slice(0, 6);
      if (!items.length) {
        closeSkillSuggest();
        return;
      }
      state.skillSuggest = { open: true, items, active: -1, trigger };
      renderSkillSuggest();
    }
    function renderSkillSuggest() {
      const box = $('skillSuggest');
      const suggest = state.skillSuggest || {};
      box.innerHTML = '';
      if (!suggest.open || !suggest.items || !suggest.items.length) {
        box.classList.remove('open');
        return;
      }
      box.classList.add('open');
      suggest.items.forEach((skill, index) => {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'skill-suggest-item' + (index === suggest.active ? ' active' : '');
        button.setAttribute('role', 'option');
        const description = skill.short_description || skill.description || '';
        button.innerHTML = '<span class="skill-suggest-name">' + escapeHTML(suggest.trigger.prefix + skill.name) + '</span><span class="skill-suggest-desc">' + escapeHTML(description) + '</span>';
        button.onmousedown = (event) => event.preventDefault();
        button.onclick = () => chooseSkillSuggest(index);
        box.appendChild(button);
      });
      const active = box.querySelector('.skill-suggest-item.active');
      if (active) active.scrollIntoView({ block: 'nearest' });
    }
    function closeSkillSuggest() {
      state.skillSuggest = { open: false, items: [], active: -1, trigger: null };
      const box = $('skillSuggest');
      if (box) {
        box.classList.remove('open');
        box.innerHTML = '';
      }
    }
    function moveSkillSuggest(delta) {
      const suggest = state.skillSuggest;
      if (!suggest.open || !suggest.items.length) return false;
      if (suggest.active < 0) {
        suggest.active = delta < 0 ? suggest.items.length - 1 : 0;
      } else {
        suggest.active = (suggest.active + delta + suggest.items.length) % suggest.items.length;
      }
      renderSkillSuggest();
      return true;
    }
    function chooseSkillSuggest(index) {
      const suggest = state.skillSuggest;
      if (!suggest.open || !suggest.items.length) return false;
      const skill = suggest.items[index >= 0 ? index : suggest.active];
      const trigger = suggest.trigger;
      if (!skill || !trigger) return false;
      const input = $('agentMessage');
      const value = input.value;
      const replacement = trigger.prefix + skill.name + ' ';
      input.value = value.slice(0, trigger.start) + replacement + value.slice(trigger.end);
      const cursor = trigger.start + replacement.length;
      input.focus();
      input.setSelectionRange(cursor, cursor);
      closeSkillSuggest();
      return true;
    }
    $('agentMessage').addEventListener('input', () => {
      updateSkillSuggest();
      renderContextTokenUsage();
    });
    $('agentMessage').addEventListener('blur', () => {
      setTimeout(closeSkillSuggest, 120);
    });
    $('attachAgentFile').onclick = () => $('agentFileInput').click();
    $('agentModel').onchange = () => { syncEffortOptionsForSelectedModel('medium'); renderContextTokenUsage(); saveAgentModelSettings(); };
    $('agentThinkingEffort').onchange = saveAgentModelSettings;
    $('agentFileInput').onchange = () => {
      addAgentAttachments($('agentFileInput').files || []);
      $('agentFileInput').value = '';
    };
