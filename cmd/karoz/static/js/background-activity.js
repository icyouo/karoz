    function backgroundProjectPath() {
      return state.project ? '/api/projects/' + encodeURIComponent(state.project.id) : '';
    }

    async function loadBackgroundActivityState(options = {}) {
      if (!state.project) return;
      const projectID = state.project.id;
      const [processPayload, monitorPayload] = await Promise.all([
        api(backgroundProjectPath() + '/processes?limit=100'),
        api(backgroundProjectPath() + '/monitors'),
      ]);
      if (!state.project || state.project.id !== projectID) return;
      state.backgroundProcesses = (processPayload && processPayload.processes) || [];
      state.backgroundMonitors = (monitorPayload && monitorPayload.monitors) || [];
      state.backgroundProbeSupported = monitorPayload ? monitorPayload.script_probe_supported !== false : true;
      if (options.render !== false && state.sidePanel === 'background' && !state.backgroundEditor) renderSidePane();
      renderRuntimeStrip();
    }

    function scheduleBackgroundActivityRefresh() {
      if (!state.project) return;
      clearTimeout(backgroundActivityRefreshTimer);
      backgroundActivityRefreshTimer = setTimeout(() => {
        backgroundActivityRefreshTimer = null;
        void loadBackgroundActivityState().catch(() => {});
      }, 160);
    }

    function backgroundOwnerLabel(agentID) {
      const agent = (state.agents || []).find(item => item.id === agentID);
      return agent ? (agent.nickname || agent.display_name || agent.name || agent.id) : agentID || 'Unknown';
    }

    function backgroundDuration(ms) {
      const value = Math.max(0, Number(ms || 0));
      if (value < 1000) return value + ' ms';
      if (value < 60000) return (value / 1000).toFixed(value < 10000 ? 1 : 0) + ' s';
      if (value < 3600000) return Math.floor(value / 60000) + 'm ' + Math.floor((value % 60000) / 1000) + 's';
      return Math.floor(value / 3600000) + 'h ' + Math.floor((value % 3600000) / 60000) + 'm';
    }

    function backgroundBytes(bytes) {
      const value = Math.max(0, Number(bytes || 0));
      if (value < 1024) return value + ' B';
      if (value < 1024 * 1024) return (value / 1024).toFixed(1) + ' KiB';
      return (value / (1024 * 1024)).toFixed(1) + ' MiB';
    }

    function backgroundDate(value) {
      if (!value) return '—';
      const date = new Date(value);
      return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });
    }

    function backgroundTriggerLabel(monitor) {
      const trigger = monitor.trigger || {};
      switch (trigger.kind) {
      case 'runtime_event':
        return 'Runtime event · ' + (trigger.event_kinds || []).join(', ')
          + (trigger.entity_id ? ' · ' + trigger.entity_id : '')
          + (trigger.from_state || trigger.to_state ? ' · ' + (trigger.from_state || '*') + ' → ' + (trigger.to_state || '*') : '');
      case 'process_exit':
        return 'Process exit · ' + trigger.process_id + (trigger.failure_only ? ' · failures only' : '');
      case 'process_output':
        return 'Process output · ' + trigger.process_id + ' · /' + (trigger.pattern || '') + '/';
      case 'script_probe':
        return 'Temporary code · ' + (trigger.probe_language || 'probe') + ' · every ' + backgroundDuration(trigger.interval_ms);
      default:
        return String(trigger.kind || 'Unknown trigger');
      }
    }

    function backgroundActionLabel(monitor) {
      const action = monitor.action || {};
      if (action.kind === 'notify_agent') {
        return 'Notify ' + backgroundOwnerLabel(action.agent_id) + ' · ' + (action.turn_type || 'ask');
      }
      return 'Blackboard · ' + (action.topic || 'monitor event');
    }

    function monitorHasUnacknowledgedGaps(monitor) {
      return Object.values(monitor.source_gaps || {}).some(gap =>
        Number(gap.acknowledged_gap_version || 0) !== Number(gap.gap_version || 0)
      );
    }

    function backgroundMonitorCapabilities(monitor, probeSupported) {
      const scriptProbe = !!(monitor && monitor.trigger && monitor.trigger.kind === 'script_probe');
      const unsupportedProbe = scriptProbe &&
        (!probeSupported || monitor.error_code === 'unsupported_platform');
      return {
        canRunCheck: scriptProbe && !unsupportedProbe,
        runCheckTitle: unsupportedProbe
          ? 'Temporary code probes are not supported in v1 on Windows'
          : (scriptProbe ? '' : 'Run check is available for temporary-code probes'),
        canResume: !unsupportedProbe,
        resumeTitle: unsupportedProbe
          ? 'Temporary code probes are not supported in v1 on Windows'
          : '',
      };
    }

    function backgroundScriptProbeEditorPresentation(monitor) {
      const trigger = (monitor && monitor.trigger) || {};
      return {
        optionLabel: 'Temporary code · immutable',
        summary: (trigger.probe_language || 'probe') + ' · every ' + backgroundDuration(trigger.interval_ms)
          + ' · timeout ' + backgroundDuration(trigger.timeout_ms),
        binding: 'Bound to MonitorID ' + (monitor.id || '') + ' · revision '
          + String(trigger.revision || monitor.revision || 0),
      };
    }

    function backgroundProcessForMonitor(monitor) {
      const processID = monitor && monitor.trigger && monitor.trigger.process_id;
      return (state.backgroundProcesses || []).find(item => item.id === processID) || null;
    }

    function renderBackgroundActivityPane(body) {
      $('sidePaneTitle').textContent = 'Background activity';
      const processes = state.backgroundProcesses || [];
      const monitors = state.backgroundMonitors || [];
      body.innerHTML = '<div class="background-activity">'
        + '<div class="background-summary"><strong>' + processes.length + ' processes · ' + monitors.length + ' monitors</strong><span>Persistent project activity on this Karoz server</span></div>'
        + '<div class="background-tabs" role="tablist" aria-label="Background activity view">'
        + '<button type="button" data-background-view="processes" class="' + (state.backgroundActivityView === 'processes' ? 'active' : '') + '">Processes</button>'
        + '<button type="button" data-background-view="monitors" class="' + (state.backgroundActivityView === 'monitors' ? 'active' : '') + '">Monitors</button>'
        + '</div>'
        + '<div id="backgroundActivityBody" class="background-activity-body custom-scrollbar"></div>'
        + '</div>';
      body.querySelectorAll('[data-background-view]').forEach(button => {
        button.onclick = () => {
          state.backgroundActivityView = button.dataset.backgroundView;
          state.backgroundEditor = null;
          state.backgroundLog = null;
          renderBackgroundActivityPane(body);
        };
      });
      if (state.backgroundActivityView === 'monitors') renderBackgroundMonitors();
      else renderBackgroundProcesses();
    }

    function renderBackgroundProcesses() {
      const body = $('backgroundActivityBody');
      if (state.backgroundLog) {
        renderBackgroundProcessLog(body);
        return;
      }
      const processes = state.backgroundProcesses || [];
      if (!processes.length) {
        body.innerHTML = '<div class="background-empty"><strong>No background processes</strong><span>A dev agent can start a resident process. It will remain owned by the Karoz server after the turn or browser closes.</span></div>';
        return;
      }
      body.innerHTML = '';
      processes.forEach(process => {
        const card = document.createElement('article');
        card.className = 'background-card';
        card.dataset.state = process.state || '';
        const running = !process.terminal;
        const statusNote = running
          ? 'Runs on the Karoz server; closing this browser will not stop it.'
          : process.state === 'interrupted'
            ? 'Karoz restarted; process was not resumed.'
            : (process.error || (process.succeeded ? 'Completed successfully.' : 'Process finished.'));
        const exit = process.terminal ? String(process.exit_code) : '—';
        card.innerHTML = '<div class="background-card-head"><span class="background-state">' + escapeHTML(process.state || 'unknown') + '</span><span>' + escapeHTML(backgroundDuration(process.runtime_ms)) + '</span></div>'
          + '<h3 title="' + escapeHTML(process.command_summary || '') + '">' + escapeHTML(process.description || process.command_summary || process.id) + '</h3>'
          + '<div class="background-command">' + escapeHTML(process.command_summary || '') + '</div>'
          + '<dl class="background-facts"><div><dt>Owner</dt><dd>' + escapeHTML(backgroundOwnerLabel(process.agent_id)) + '</dd></div><div><dt>Lifetime</dt><dd>' + escapeHTML(process.lifetime_ms === 0 ? 'Unlimited' : backgroundDuration(process.lifetime_ms)) + '</dd></div><div><dt>Exit</dt><dd>' + escapeHTML(exit) + '</dd></div><div><dt>Log</dt><dd>' + escapeHTML(backgroundBytes(process.log_bytes)) + ' · ' + escapeHTML(String(process.log_lines || 0)) + ' lines</dd></div></dl>'
          + (process.last_line ? '<div class="background-last-line"><span>Last line</span><code>' + escapeHTML(process.last_line) + '</code></div>' : '')
          + '<p class="background-note">' + escapeHTML(statusNote) + '</p>'
          + '<div class="background-actions"><button type="button" class="secondary" data-process-log="' + escapeHTML(process.id) + '">View log</button>'
          + (running ? '<button type="button" class="danger" data-process-stop="' + escapeHTML(process.id) + '">Stop</button>' : '')
          + '</div>';
        body.appendChild(card);
      });
      body.querySelectorAll('[data-process-log]').forEach(button => {
        button.onclick = () => void openBackgroundProcessLog(button.dataset.processLog);
      });
      body.querySelectorAll('[data-process-stop]').forEach(button => {
        button.onclick = () => void stopBackgroundProcess(button.dataset.processStop);
      });
    }

    async function openBackgroundProcessLog(processID) {
      const payload = await api(backgroundProjectPath() + '/processes/' + encodeURIComponent(processID) + '/log?limit=200&tail=true');
      state.backgroundLog = { processID, window: payload.log || { lines: [] } };
      renderSidePane();
    }

    function renderBackgroundProcessLog(body) {
      const entry = state.backgroundLog || {};
      const process = (state.backgroundProcesses || []).find(item => item.id === entry.processID);
      const window = entry.window || {};
      body.innerHTML = '<div class="background-log-head"><button type="button" class="secondary" id="backgroundLogBack">← Processes</button><strong>' + escapeHTML(process ? (process.description || process.id) : entry.processID) + '</strong><span>' + escapeHTML(String(window.lines ? window.lines.length : 0)) + ' lines</span></div>'
        + '<pre class="background-log custom-scrollbar"></pre>';
      body.querySelector('.background-log').textContent = (window.lines || []).join('\n');
      $('backgroundLogBack').onclick = () => {
        state.backgroundLog = null;
        renderSidePane();
      };
    }

    async function stopBackgroundProcess(processID) {
      const confirmed = await requestConfirmation({
        title: 'Stop background process?',
        message: 'Karoz will stop this process and its descendants.',
        confirmLabel: 'Stop process',
        danger: true,
      });
      if (!confirmed) return;
      await api(backgroundProjectPath() + '/processes/' + encodeURIComponent(processID) + '/stop', {
        method: 'POST',
        body: JSON.stringify({}),
      });
      await loadBackgroundActivityState();
      notify('Background process stopped.', 'success');
    }

    function renderBackgroundMonitors() {
      const body = $('backgroundActivityBody');
      if (state.backgroundEditor) {
        renderBackgroundMonitorEditor(body);
        return;
      }
      const monitors = state.backgroundMonitors || [];
      body.innerHTML = '<div class="background-toolbar"><div><button type="button" id="backgroundCreateMonitor">New monitor</button><button type="button" class="secondary" id="backgroundPrepareProbe">Advanced · Temporary code</button></div><span>' + (state.backgroundProbeSupported ? 'Unix v1' : 'Not supported in v1 on Windows') + '</span></div><div id="backgroundMonitorList"></div>';
      $('backgroundCreateMonitor').onclick = () => {
        state.backgroundEditor = { mode: 'create' };
        renderSidePane();
      };
      $('backgroundPrepareProbe').disabled = !state.backgroundProbeSupported;
      $('backgroundPrepareProbe').onclick = () => {
        state.backgroundEditor = { mode: 'probe' };
        state.backgroundProbeApproval = null;
        renderSidePane();
      };
      const list = $('backgroundMonitorList');
      if (!monitors.length) {
        list.innerHTML = '<div class="background-empty"><strong>No monitors</strong><span>Create a no-code event monitor, or prepare temporary-code approval for a dev agent turn.</span></div>';
        return;
      }
      monitors.forEach(monitor => list.appendChild(backgroundMonitorCard(monitor)));
    }

    function backgroundMonitorCard(monitor) {
      const card = document.createElement('article');
      card.className = 'background-card monitor-card';
      card.dataset.state = monitor.state || '';
      const process = backgroundProcessForMonitor(monitor);
      const sourceGaps = Object.values(monitor.source_gaps || {});
      const unacknowledged = monitorHasUnacknowledgedGaps(monitor);
      const capabilities = backgroundMonitorCapabilities(monitor, state.backgroundProbeSupported);
      const outputCoverage = monitor.trigger && monitor.trigger.kind === 'process_output' && process
        ? '<div class="background-coverage"><strong>Live / best effort</strong><span>' + Number(process.output_lost_lines || 0) + ' lost lines · ' + Number(process.output_gap_count || 0) + ' gaps</span>'
          + (Number(process.output_lost_lines || 0) ? '<em>Coverage degraded</em>' : '')
          + (process.output_gaps || []).map(gap => '<code>' + escapeHTML(String(gap.start)) + '–' + escapeHTML(String(gap.end)) + '</code>').join('')
          + '</div>'
        : '';
      const sourceGapRows = sourceGaps.length
        ? '<div class="source-gap-list">' + sourceGaps.map(gap => {
          const acknowledged = Number(gap.acknowledged_gap_version || 0) === Number(gap.gap_version || 0);
          return '<div class="source-gap-row"><span>' + escapeHTML(gap.source_kind) + ' · ' + escapeHTML(gap.authority_id) + '</span><code>' + escapeHTML(String(gap.first_version)) + '–' + escapeHTML(String(gap.last_version)) + ' · ' + escapeHTML(String(gap.lost_count)) + ' lost</code>'
            + (acknowledged ? '<strong>Acknowledged</strong>' : '<button type="button" class="secondary" data-monitor-gap="' + escapeHTML(monitor.id) + '" data-authority="' + escapeHTML(gap.authority_id) + '" data-source-kind="' + escapeHTML(gap.source_kind) + '" data-gap-version="' + escapeHTML(String(gap.gap_version)) + '">Acknowledge</button>')
            + '</div>';
        }).join('') + '</div>'
        : '';
      const dryRunResult = (state.backgroundCheckResults || {})[monitor.id];
      const actionResult = dryRunResult
        ? '<div class="background-result"><strong>Dry-run · action not executed</strong><span>' + escapeHTML(dryRunResult) + '</span></div>'
        : '';
      card.innerHTML = '<div class="background-card-head"><span class="background-state">' + escapeHTML(monitor.state || 'unknown') + '</span><span>match ' + escapeHTML(String(monitor.trigger_count || 0)) + '</span></div>'
        + '<h3>' + escapeHTML(monitor.name || monitor.id) + '</h3>'
        + '<p class="background-human">' + escapeHTML(backgroundTriggerLabel(monitor)) + '</p>'
        + '<p class="background-human">' + escapeHTML(backgroundActionLabel(monitor)) + '</p>'
        + '<dl class="background-facts"><div><dt>Owner</dt><dd>' + escapeHTML(backgroundOwnerLabel(monitor.agent_id)) + '</dd></div><div><dt>Last</dt><dd>' + escapeHTML(backgroundDate(monitor.last_checked_at || monitor.last_fired_at)) + '</dd></div><div><dt>Next</dt><dd>' + escapeHTML(backgroundDate(monitor.next_check_at)) + '</dd></div><div><dt>Cooldown</dt><dd>' + escapeHTML(backgroundDuration(monitor.cooldown_ms)) + '</dd></div><div><dt>Expiry</dt><dd>' + escapeHTML(backgroundDate(monitor.expires_at)) + '</dd></div></dl>'
        + (monitor.last_match ? '<div class="background-last-line"><span>Last match</span><code>' + escapeHTML(monitor.last_match) + '</code></div>' : '')
        + (monitor.last_error ? '<p class="background-note error">' + escapeHTML(monitor.last_error) + '</p>' : '')
        + outputCoverage + sourceGapRows + actionResult
        + '<div class="background-actions"><button type="button" class="secondary" data-monitor-toggle="' + escapeHTML(monitor.id) + '" data-operation="' + (monitor.state === 'active' ? 'pause' : 'resume') + '"' + (monitor.state !== 'active' && (unacknowledged || !capabilities.canResume) ? ' disabled title="' + escapeHTML(!capabilities.canResume ? capabilities.resumeTitle : 'Acknowledge every source gap before resuming') + '"' : '') + '>' + (monitor.state === 'active' ? 'Pause' : 'Resume') + '</button>'
        + '<button type="button" class="secondary" data-monitor-edit="' + escapeHTML(monitor.id) + '">Edit</button>'
        + '<button type="button" class="secondary" data-monitor-check="' + escapeHTML(monitor.id) + '"' + (!capabilities.canRunCheck ? ' disabled title="' + escapeHTML(capabilities.runCheckTitle) + '"' : '') + '>Run check</button>'
        + '<button type="button" class="danger" data-monitor-delete="' + escapeHTML(monitor.id) + '">Delete</button></div>';
      card.querySelectorAll('[data-monitor-toggle]').forEach(button => {
        button.onclick = () => void mutateBackgroundMonitor(monitor, button.dataset.operation);
      });
      card.querySelectorAll('[data-monitor-edit]').forEach(button => {
        button.onclick = () => {
          state.backgroundEditor = { mode: 'edit', monitorID: button.dataset.monitorEdit };
          renderSidePane();
        };
      });
      card.querySelectorAll('[data-monitor-check]').forEach(button => {
        button.onclick = () => void checkBackgroundMonitor(monitor);
      });
      card.querySelectorAll('[data-monitor-delete]').forEach(button => {
        button.onclick = () => void deleteBackgroundMonitor(monitor);
      });
      card.querySelectorAll('[data-monitor-gap]').forEach(button => {
        button.onclick = () => void acknowledgeBackgroundMonitorGap(monitor, button.dataset);
      });
      return card;
    }

    async function mutateBackgroundMonitor(monitor, operation) {
      await api(backgroundProjectPath() + '/monitors/' + encodeURIComponent(monitor.id) + '/' + operation, {
        method: 'POST',
        body: JSON.stringify({}),
      });
      await loadBackgroundActivityState();
      notify(operation === 'pause' ? 'Monitor paused.' : 'Monitor resumed.', 'success');
    }

    async function checkBackgroundMonitor(monitor) {
      const payload = await api(backgroundProjectPath() + '/monitors/' + encodeURIComponent(monitor.id) + '/check', {
        method: 'POST',
        body: JSON.stringify({}),
      });
      const result = payload && payload.result;
      state.backgroundCheckResults[monitor.id] = result
        ? (result.error || (result.matched ? 'Matched' + (result.detail ? ' · ' + result.detail : '') : 'No match'))
        : 'No result';
      renderSidePane();
    }

    async function deleteBackgroundMonitor(monitor) {
      const confirmed = await requestConfirmation({
        title: 'Delete monitor?',
        message: 'Delete “' + (monitor.name || monitor.id) + '” and its pending activity.',
        confirmLabel: 'Delete monitor',
        danger: true,
      });
      if (!confirmed) return;
      await api(backgroundProjectPath() + '/monitors/' + encodeURIComponent(monitor.id), {
        method: 'DELETE',
        body: JSON.stringify({}),
      });
      delete state.backgroundCheckResults[monitor.id];
      await loadBackgroundActivityState();
      notify('Monitor deleted.', 'success');
    }

    async function acknowledgeBackgroundMonitorGap(monitor, data) {
      await api(backgroundProjectPath() + '/monitors/' + encodeURIComponent(monitor.id) + '/acknowledge-gap', {
        method: 'POST',
        body: JSON.stringify({
          authority_id: data.authority,
          source_kind: data.sourceKind,
          expected_gap_version: Number(data.gapVersion),
        }),
      });
      await loadBackgroundActivityState();
      notify('Coverage loss acknowledged. Resume remains a separate action.', 'success');
    }

    function renderBackgroundMonitorEditor(body) {
      const editor = state.backgroundEditor || { mode: 'create' };
      if (editor.mode === 'probe') {
        renderBackgroundProbeApproval(body);
        return;
      }
      const monitor = editor.mode === 'edit'
        ? (state.backgroundMonitors || []).find(item => item.id === editor.monitorID)
        : null;
      if (editor.mode === 'edit' && !monitor) {
        state.backgroundEditor = null;
        renderBackgroundMonitors();
        return;
      }
      const trigger = (monitor && monitor.trigger) || { kind: 'runtime_event', event_kinds: ['task_changed'] };
      const action = (monitor && monitor.action) || { kind: 'notify_agent', agent_id: currentAgentID(), turn_type: 'ask' };
      const processOptions = (state.backgroundProcesses || []).filter(item => !item.terminal || item.id === trigger.process_id)
        .map(item => '<option value="' + escapeHTML(item.id) + '"' + (item.id === trigger.process_id ? ' selected' : '') + '>' + escapeHTML(item.description || item.command_summary || item.id) + '</option>').join('');
      const agentOptions = (state.agents || []).map(agent => '<option value="' + escapeHTML(agent.id) + '"' + (agent.id === action.agent_id ? ' selected' : '') + '>' + escapeHTML(backgroundOwnerLabel(agent.id)) + '</option>').join('');
      const scriptProbePresentation = monitor && trigger.kind === 'script_probe'
        ? backgroundScriptProbeEditorPresentation(monitor)
        : null;
      const scriptProbeOption = monitor && trigger.kind === 'script_probe'
        ? '<option value="script_probe" selected>' + escapeHTML(scriptProbePresentation.optionLabel) + '</option>'
        : '';
      body.innerHTML = '<form id="backgroundMonitorForm" class="background-editor">'
        + '<div class="background-editor-head"><button type="button" class="secondary" id="backgroundEditorBack">← Monitors</button><strong>' + (monitor ? 'Edit monitor' : 'New monitor') + '</strong></div>'
        + '<label>Name<input name="name" maxlength="160" required value="' + escapeHTML((monitor && monitor.name) || '') + '"></label>'
        + '<label>Trigger<select name="trigger_kind"' + (monitor ? ' disabled title="Trigger changes require a dev agent turn"' : '') + '><option value="runtime_event"' + (trigger.kind === 'runtime_event' ? ' selected' : '') + '>Runtime event</option><option value="process_exit"' + (trigger.kind === 'process_exit' ? ' selected' : '') + '>Process exit</option><option value="process_output"' + (trigger.kind === 'process_output' ? ' selected' : '') + '>Process output · Live / best effort</option>' + scriptProbeOption + '</select></label>'
        + (monitor ? '<p class="background-note">The trigger is revision-bound. Edit it from a dev agent turn; this form updates presentation, action, and guards only.</p>' : '')
        + '<div id="backgroundTriggerFields"></div>'
        + '<label>Action<select name="action_kind"><option value="notify_agent"' + (action.kind === 'notify_agent' ? ' selected' : '') + '>Notify agent</option><option value="blackboard"' + (action.kind === 'blackboard' ? ' selected' : '') + '>Write to blackboard</option></select></label>'
        + '<div id="backgroundActionFields"></div>'
        + '<label>Briefing template<textarea name="template" maxlength="16384" placeholder="Describe what matched and what should happen next.">' + escapeHTML(action.template || '') + '</textarea></label>'
        + '<div class="background-editor-grid"><label>Cooldown (ms)<input name="cooldown_ms" type="number" min="5000" value="' + escapeHTML(String((monitor && monitor.cooldown_ms) || 60000)) + '"></label><label>Maximum matches<input name="max_triggers" type="number" min="0" value="' + escapeHTML(String((monitor && monitor.max_triggers) || 0)) + '"></label></div>'
        + '<div class="background-actions"><button type="submit">' + (monitor ? 'Save changes' : 'Create monitor') + '</button><button type="button" class="secondary" id="backgroundEditorCancel">Cancel</button></div>'
        + '</form>';
      const form = $('backgroundMonitorForm');
      const renderTriggerFields = () => {
        const kind = form.elements.trigger_kind.value;
        const fields = $('backgroundTriggerFields');
        if (kind === 'runtime_event') {
          fields.innerHTML = '<label>Event kinds<input name="event_kinds" required value="' + escapeHTML((trigger.event_kinds || ['task_changed']).join(', ')) + '" placeholder="task_changed, handoff_changed"></label><label>Entity ID (optional)<input name="entity_id" value="' + escapeHTML(trigger.entity_id || '') + '"></label><div class="background-editor-grid"><label>From state (optional)<input name="from_state" value="' + escapeHTML(trigger.from_state || '') + '"></label><label>To state (optional)<input name="to_state" value="' + escapeHTML(trigger.to_state || '') + '"></label></div><label class="background-check"><input name="include_monitor_events" type="checkbox"' + (trigger.include_monitor_events ? ' checked' : '') + '> Include events produced by other monitors</label>';
        } else if (kind === 'process_exit') {
          fields.innerHTML = '<label>Process<select name="process_id" required>' + processOptions + '</select></label><label class="background-check"><input name="failure_only" type="checkbox"' + (trigger.failure_only ? ' checked' : '') + '> Match failures only</label>';
        } else if (kind === 'process_output') {
          fields.innerHTML = '<label>Process<select name="process_id" required>' + processOptions + '</select></label><label>Line pattern (regular expression)<input name="pattern" required value="' + escapeHTML(trigger.pattern || '') + '" placeholder="ready|listening"></label><p class="background-note">Only complete, redacted lines are evaluated. Delivery is live and best effort.</p>';
        } else {
          fields.innerHTML = '<div class="background-approval"><strong>Temporary code · immutable trigger</strong><span>' + escapeHTML(scriptProbePresentation.summary) + '</span><span>' + escapeHTML(scriptProbePresentation.binding) + '</span><span>Trigger source and execution settings can only be replaced from a dev agent turn.</span></div>';
        }
      };
      const renderActionFields = () => {
        const fields = $('backgroundActionFields');
        if (form.elements.action_kind.value === 'notify_agent') {
          fields.innerHTML = '<label>Target agent<select name="action_agent_id" required>' + agentOptions + '</select></label><label>Turn type<select name="turn_type"><option value="ask"' + (action.turn_type !== 'plan' ? ' selected' : '') + '>Ask</option><option value="plan"' + (action.turn_type === 'plan' ? ' selected' : '') + '>Plan</option></select></label>';
        } else {
          fields.innerHTML = '<label>Blackboard topic<input name="topic" required value="' + escapeHTML(action.topic || '') + '" placeholder="Background activity"></label>';
        }
      };
      renderTriggerFields();
      renderActionFields();
      form.elements.trigger_kind.onchange = renderTriggerFields;
      form.elements.action_kind.onchange = renderActionFields;
      $('backgroundEditorBack').onclick = $('backgroundEditorCancel').onclick = () => {
        state.backgroundEditor = null;
        renderSidePane();
      };
      form.onsubmit = event => {
        event.preventDefault();
        void saveBackgroundMonitor(form, monitor);
      };
    }

    async function saveBackgroundMonitor(form, monitor) {
      const data = new FormData(form);
      const actionKind = data.get('action_kind');
      const action = {
        revision: monitor ? Number((monitor.action && monitor.action.revision) || 1) : 1,
        kind: actionKind,
        template: String(data.get('template') || ''),
      };
      if (actionKind === 'notify_agent') {
        action.agent_id = String(data.get('action_agent_id') || '');
        action.turn_type = String(data.get('turn_type') || 'ask');
      } else {
        action.topic = String(data.get('topic') || '');
      }
      if (monitor) {
        await api(backgroundProjectPath() + '/monitors/' + encodeURIComponent(monitor.id), {
          method: 'PATCH',
          body: JSON.stringify({
            revision: monitor.revision,
            name: String(data.get('name') || ''),
            action,
            cooldown_ms: Number(data.get('cooldown_ms') || 0),
            max_triggers: Number(data.get('max_triggers') || 0),
          }),
        });
      } else {
        const kind = String(data.get('trigger_kind') || '');
        const trigger = { revision: 1, kind };
        if (kind === 'runtime_event') {
          trigger.event_kinds = String(data.get('event_kinds') || '').split(',').map(value => value.trim()).filter(Boolean);
          trigger.entity_id = String(data.get('entity_id') || '').trim();
          trigger.from_state = String(data.get('from_state') || '').trim();
          trigger.to_state = String(data.get('to_state') || '').trim();
          trigger.include_monitor_events = data.get('include_monitor_events') === 'on';
        } else {
          trigger.process_id = String(data.get('process_id') || '');
          if (kind === 'process_exit') trigger.failure_only = data.get('failure_only') === 'on';
          else trigger.pattern = String(data.get('pattern') || '');
        }
        await api(backgroundProjectPath() + '/monitors', {
          method: 'POST',
          body: JSON.stringify({
            agent_id: currentAgentID(),
            name: String(data.get('name') || ''),
            trigger,
            action,
            state: 'active',
            cooldown_ms: Number(data.get('cooldown_ms') || 0),
            max_triggers: Number(data.get('max_triggers') || 0),
          }),
        });
      }
      state.backgroundEditor = null;
      await loadBackgroundActivityState();
      notify(monitor ? 'Monitor updated.' : 'Monitor created.', 'success');
    }

    function renderBackgroundProbeApproval(body) {
      const approval = state.backgroundProbeApproval;
      const agentOptions = (state.agents || []).map(agent => '<option value="' + escapeHTML(agent.id) + '"' + (agent.id === currentAgentID() ? ' selected' : '') + '>' + escapeHTML(backgroundOwnerLabel(agent.id)) + '</option>').join('');
      body.innerHTML = '<form id="backgroundProbeForm" class="background-editor">'
        + '<div class="background-editor-head"><button type="button" class="secondary" id="backgroundProbeBack">← Monitors</button><strong>Advanced · Temporary code</strong></div>'
        + (!state.backgroundProbeSupported ? '<div class="background-empty"><strong>Not supported in v1 on Windows</strong><span>Existing records remain visible but cannot execute.</span></div>' : '')
        + '<p class="background-note">Preparation only. This UI cannot create, replace, claim, or enable a script probe.</p>'
        + '<label>Owner<select name="agent_id" required>' + agentOptions + '</select></label>'
        + '<label>Existing Monitor ID (optional)<input name="monitor_id" value=""></label>'
        + '<label>Language<select name="language"><option value="shell">Shell</option><option value="javascript">JavaScript</option></select></label>'
        + '<label>Project workdir<input name="workdir" value="' + escapeHTML((state.project && state.project.path) || '') + '"></label>'
        + '<label>Source<textarea name="source" maxlength="65536" required></textarea></label>'
        + '<div class="background-editor-grid"><label>Interval (ms)<input name="interval_ms" type="number" min="10000" value="60000"></label><label>Timeout (ms)<input name="timeout_ms" type="number" min="1" max="30000" value="5000"></label></div>'
        + (approval ? '<div class="background-approval"><strong>Bound MonitorID ' + escapeHTML(approval.monitor_id) + ' · revision ' + escapeHTML(String(approval.trigger_revision)) + '</strong><span>SHA-256 ' + escapeHTML(approval.source_sha256 || '') + '</span><span>Complete creation in a dev agent turn.</span></div>' : '')
        + '<div class="background-actions"><button type="submit"' + (!state.backgroundProbeSupported ? ' disabled' : '') + '>' + (approval ? 'Prepare another' : 'Prepare approval') + '</button>'
        + (approval && approval.challenge_id ? '<button type="button" id="backgroundProbeConfirm">Confirm approval</button>' : '')
        + '</div></form>';
      $('backgroundProbeBack').onclick = () => {
        state.backgroundEditor = null;
        state.backgroundProbeApproval = null;
        renderSidePane();
      };
      const form = $('backgroundProbeForm');
      form.onsubmit = event => {
        event.preventDefault();
        void prepareBackgroundProbe(form);
      };
      if ($('backgroundProbeConfirm')) {
        $('backgroundProbeConfirm').onclick = () => void confirmBackgroundProbe(approval.challenge_id);
      }
    }

    async function prepareBackgroundProbe(form) {
      const data = new FormData(form);
      const payload = await api(backgroundProjectPath() + '/monitor-probe-approvals/prepare', {
        method: 'POST',
        body: JSON.stringify({
          agent_id: String(data.get('agent_id') || ''),
          monitor_id: String(data.get('monitor_id') || '').trim(),
          language: String(data.get('language') || ''),
          workdir: String(data.get('workdir') || ''),
          source: String(data.get('source') || ''),
          interval_ms: Number(data.get('interval_ms') || 0),
          timeout_ms: Number(data.get('timeout_ms') || 0),
        }),
      });
      state.backgroundProbeApproval = payload.challenge || null;
      renderSidePane();
    }

    async function confirmBackgroundProbe(challengeID) {
      const payload = await api(backgroundProjectPath() + '/monitor-probe-approvals/' + encodeURIComponent(challengeID) + '/confirm', {
        method: 'POST',
        body: JSON.stringify({}),
      });
      const receipt = payload && payload.receipt;
      state.backgroundProbeApproval = {
        ...(state.backgroundProbeApproval || {}),
        ...(receipt || {}),
        challenge_id: '',
      };
      renderSidePane();
      notify('Approval confirmed. Complete creation in a dev agent turn.', 'success');
    }

    if (typeof module !== 'undefined' && module.exports) {
      module.exports = {
        backgroundDuration,
        backgroundBytes,
        backgroundTriggerLabel,
        monitorHasUnacknowledgedGaps,
        backgroundMonitorCapabilities,
        backgroundScriptProbeEditorPresentation,
      };
    }
