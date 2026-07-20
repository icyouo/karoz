    async function loadResidentRuntimeState() {
      if (!state.project || !state.agent) return;
      const projectID = state.project.id;
      const agentID = currentAgentID();
      const agentPath = '/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID);
      const [inbox, memory, blackboard, artifactPayload, groupPayload, planPayload] = await Promise.all([
        api(agentPath + '/inbox').catch(() => []),
        api(agentPath + '/memory').catch(() => []),
        api('/api/projects/' + projectID + '/agent-blackboard').catch(() => []),
		api('/api/projects/' + projectID + '/artifacts').catch(() => ({ artifacts: [] })),
        api('/api/projects/' + projectID + '/groups').catch(() => ({ groups: [] })),
        api('/api/projects/' + projectID + '/plans').catch(() => ({ plans: [] })),
      ]);
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      state.inbox = inbox || [];
      state.memory = memory || [];
      state.blackboard = blackboard || [];
	  state.artifacts = (artifactPayload && artifactPayload.artifacts) || [];
      state.groups = (groupPayload && groupPayload.groups) || [];
      state.plans = (planPayload && planPayload.plans) || [];
      renderRuntimeStrip();
    }
    function renderRuntimeStrip() {
      const pending = (state.memory || []).filter(item => item.layer === 'pending').length;
      const boardCount = (state.blackboard || []).length;
      const role = titleCaseCompact(shortAgentRole());
      const workText = currentAgentWorking() ? ' · Working' : '';
      const chip = (panel, label, count, title, attention = 0) => '<button class="runtime-tool secondary ' + (state.sidePanel === panel ? 'active' : '') + '" data-side-panel="' + panel + '" title="' + escapeHTML(title || ('Open ' + label)) + '"><span>' + label + '</span><strong>' + count + '</strong>' + (attention > 0 ? '<i title="' + attention + ' pending" aria-label="' + attention + ' pending"></i>' : '') + '</button>';
      $('agentHeaderMeta').textContent = role + workText;
      $('agentRuntimeTools').innerHTML = chip('inbox', 'Inbox', (state.inbox || []).length)
        + chip('plans', 'Plans', (state.plans || []).filter(item => !['completed', 'cancelled'].includes(item.status)).length, 'Open WorkPlans')
		+ chip('artifacts', 'Artifacts', (state.artifacts || []).filter(item => item.status !== 'superseded').length)
        + chip('memory', 'Memory', (state.memory || []).length, pending > 0 ? ('Open Memory · ' + pending + ' pending') : 'Open Memory', pending)
        + chip('blackboard', 'Activity', boardCount, 'Open agent activity');
      $('agentHeaderMeta').title = 'inbox ' + (state.inbox || []).length + ' · memory ' + (state.memory || []).length + ' · pending ' + pending + ' · blackboard ' + boardCount + ' · archived ' + (state.archive || []).length;
      renderAgentWorkingState();
      if (['blackboard', 'artifacts', 'plans', 'inbox', 'memory', 'pending'].includes(state.sidePanel)) renderSidePane();
    }
    async function loadWorkspaceFiles() {
      if (!state.project || !state.agent) return;
      const res = await api('/api/projects/' + state.project.id + '/agents/' + encodeURIComponent(currentAgentID()) + '/workspace/files').catch(() => ({ data: [] }));
      state.workspaceFiles = res.data || [];
    }
    async function openWorkspaceFile(path) {
      if (!state.project || !state.agent) return;
      const preview = await api('/api/projects/' + state.project.id + '/agents/' + encodeURIComponent(currentAgentID()) + '/workspace/file?path=' + encodeURIComponent(path));
      state.preview = preview;
      state.sidePanel = 'preview';
      renderSidePane();
    }
    function closeSidePane() {
      state.sidePanel = null;
      renderSidePane();
      renderRuntimeStrip();
    }
    async function openRuntimePanel(panel) {
      if (state.sidePanel === panel) {
        closeSidePane();
        return;
      }
      state.sidePanel = panel;
      renderSidePane();
      await loadResidentRuntimeState();
      state.sidePanel = panel;
      renderSidePane();
      renderRuntimeStrip();
    }
    function renderSidePane() {
      const shell = $('agentChatShell');
      const body = $('sidePaneBody');
      const open = !!state.sidePanel && (state.sidePanel !== 'preview' || !!state.preview);
      shell.classList.toggle('side-open', open);
      $('togglePreviewPane').classList.toggle('active', state.sidePanel === 'preview' && !!state.preview);
      document.querySelectorAll('#agentRuntimeTools button[data-side-panel]').forEach(button => {
        button.classList.toggle('active', state.sidePanel === button.dataset.sidePanel);
      });
      if (!open) {
        body.innerHTML = '';
        $('sidePaneTitle').textContent = 'Preview';
        return;
      }
      try {
        if (state.sidePanel === 'blackboard') {
          renderBlackboardPane(body);
          return;
        }
		if (state.sidePanel === 'artifacts') {
		  renderArtifactsPane(body);
		  return;
		}
        if (state.sidePanel === 'plans') {
          renderPlansPane(body);
          return;
        }
        if (state.sidePanel === 'inbox') {
          renderRuntimeListPane(body, 'Inbox', state.inbox || [], renderInboxEntry);
          return;
        }
        if (state.sidePanel === 'memory') {
          renderRuntimeListPane(body, 'Memory', state.memory || [], renderMemoryEntry);
          return;
        }
        if (state.sidePanel === 'pending') {
          renderRuntimeListPane(body, 'Pending', (state.memory || []).filter(item => item.layer === 'pending'), renderMemoryEntry);
          return;
        }
        renderPreviewPane(body);
      } catch (err) {
        $('sidePaneTitle').textContent = 'Panel';
        body.innerHTML = '<div class="blackboard-view"><div class="blackboard-section-title">Unable to render</div><div class="blackboard-entry"><div class="blackboard-entry-summary">' + escapeHTML(err.message || String(err)) + '</div></div></div>';
      }
    }
    function renderPreviewPane(body) {
      $('sidePaneTitle').textContent = state.preview.filename || 'Preview';
      const content = state.preview.content || '';
      const mime = state.preview.mime_type || '';
      if (/html/i.test(mime) || /\.html?$/i.test(state.preview.filename || '')) {
        body.innerHTML = '<iframe class="preview-frame" sandbox="allow-scripts allow-forms allow-same-origin"></iframe>';
        body.querySelector('iframe').srcdoc = normalizePreviewHTML(content);
      } else if (/^image\//i.test(mime)) {
        const img = document.createElement('img');
        img.alt = state.preview.filename || 'Artifact preview';
        img.style.cssText = 'display:block;max-width:100%;height:auto;margin:0 auto;';
        img.src = state.preview.encoding === 'base64' ? 'data:' + mime + ';base64,' + content : content;
        body.innerHTML = '';
        body.appendChild(img);
      } else if (/markdown/i.test(mime) || /\.md|\.markdown$/i.test(state.preview.filename || '')) {
        const article = document.createElement('article');
        article.className = 'preview-markdown';
        article.innerHTML = renderMarkdown(content);
        body.innerHTML = '';
        body.appendChild(article);
      } else {
        const pre = document.createElement('div');
        pre.className = 'preview-text';
        pre.textContent = content;
        body.innerHTML = '';
        body.appendChild(pre);
      }
    }
    function renderBlackboardPane(body) {
      $('sidePaneTitle').textContent = 'Blackboard';
      const entries = state.blackboard || [];
      const pendingInbox = (state.inbox || []).length;
      const pendingMemory = (state.memory || []).filter(item => item.layer === 'pending' && item.state !== 'archived').length;
      const latest = entries[0];
      body.innerHTML = '<div class="blackboard-view">'
        + '<div class="blackboard-state">'
        + '<div class="blackboard-metric"><label>activities</label><strong>' + entries.length + '</strong></div>'
        + '<div class="blackboard-metric"><label>inbox</label><strong>' + pendingInbox + '</strong></div>'
        + '<div class="blackboard-metric"><label>pending</label><strong>' + pendingMemory + '</strong></div>'
        + '</div>'
        + '<div class="blackboard-section-title">' + escapeHTML(latest ? 'latest · ' + (latest.activity_kind || 'activity') : 'activity log') + '</div>'
        + '<div id="blackboardLog" class="blackboard-log custom-scrollbar"></div>'
        + '</div>';
      const log = $('blackboardLog');
      if (!entries.length) {
        const empty = document.createElement('div');
        empty.className = 'muted';
        empty.style.padding = '12px';
        empty.textContent = 'No blackboard activity yet.';
        log.appendChild(empty);
        return;
      }
      entries.forEach(entry => {
        const item = document.createElement('div');
        item.className = 'blackboard-entry';
        const meta = [
          entry.agent_name || entry.agent_id || 'agent',
		  entry.source_type && entry.source_id ? entry.source_type + ' ' + entry.source_id : (entry.source_inbox_message_id ? 'inbox ' + entry.source_inbox_message_id : '')
        ].filter(Boolean).join(' · ');
        item.innerHTML = '<div class="blackboard-entry-head"><span class="blackboard-entry-kind">' + escapeHTML(entry.activity_kind || 'activity') + '</span><span class="blackboard-entry-time">' + escapeHTML(compactDate(entry.updated_at || entry.created_at)) + '</span></div>'
          + '<div class="blackboard-entry-summary">' + escapeHTML(entry.summary || '') + '</div>'
          + '<div class="blackboard-entry-meta">' + escapeHTML(meta) + '</div>'
          + (entry.detail ? '<div class="blackboard-entry-detail">' + escapeHTML(entry.detail) + '</div>' : '');
        log.appendChild(item);
      });
    }
    function renderRuntimeListPane(body, title, entries, renderEntry) {
      $('sidePaneTitle').textContent = title;
      body.innerHTML = '<div class="blackboard-view">'
        + '<div class="blackboard-state">'
        + '<div class="blackboard-metric"><label>' + escapeHTML(title) + '</label><strong>' + entries.length + '</strong></div>'
        + '<div class="blackboard-metric"><label>Agent</label><strong>' + escapeHTML(currentAgentLabel().slice(0, 12)) + '</strong></div>'
        + '<div class="blackboard-metric"><label>Updated</label><strong>' + escapeHTML(new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })) + '</strong></div>'
        + '</div>'
        + '<div class="blackboard-section-title">' + escapeHTML(title + ' items') + '</div>'
        + '<div id="runtimePanelList" class="blackboard-log custom-scrollbar"></div>'
        + '</div>';
      const list = $('runtimePanelList');
      if (!entries.length) {
        const empty = document.createElement('div');
        empty.className = 'muted';
        empty.style.padding = '12px';
        empty.textContent = 'No items.';
        list.appendChild(empty);
        return;
      }
      entries.forEach(entry => {
        const item = document.createElement('div');
        item.className = 'blackboard-entry';
        item.innerHTML = renderEntry(entry);
        list.appendChild(item);
      });
    }
	function planStatusLabel(status, step = false) {
	  const labels = step
		? { pending: 'Queued', in_progress: 'In progress', reviewing: 'In review', completed: 'Complete', blocked: 'Blocked', skipped: 'Skipped' }
		: { draft: 'Draft', awaiting_approval: 'Awaiting approval', active: 'Active', paused: 'Paused', completed: 'Complete', cancelled: 'Cancelled', failed: 'Failed' };
	  return labels[status] || titleCaseCompact(String(status || (step ? 'pending' : 'draft')).replace(/_/g, ' '));
	}
	function compactPlanActor(agentID) {
	  if (!agentID) return 'Unassigned';
	  const agent = (state.agents || []).find(entry => entry.id === agentID);
	  if (agent) return agent.nickname || agent.display_name || agent.short_name || agent.name || agentID;
	  const parts = String(agentID).split('-').filter(Boolean);
	  return titleCaseCompact(parts[parts.length - 1] || agentID);
	}
	function planStepMarker(status) {
	  return status === 'completed' ? '✓' : status === 'reviewing' ? '◇' : status === 'in_progress' ? '•' : status === 'blocked' ? '!' : status === 'skipped' ? '—' : '';
	}
	function planStepMeta(step) {
	  const status = step.status || 'pending';
	  if (status === 'reviewing') {
		const pendingReview = (step.reviews || []).find(review => review.status === 'pending');
		return pendingReview ? 'Review pending · ' + compactPlanActor(pendingReview.reviewer_agent_id) : 'Awaiting review';
	  }
	  if (status === 'pending' && (step.dependencies || []).length) return 'Waiting on ' + step.dependencies.join(', ');
	  return planStatusLabel(status, true) + ' · ' + compactPlanActor(step.assigned_agent_id);
	}
	function conflictingGroupPlan(plan) {
	  if (!plan || plan.owner_type !== 'group' || !plan.owner_group_id) return null;
	  return (state.plans || []).find(candidate => candidate.id !== plan.id && candidate.owner_type === 'group' && candidate.owner_group_id === plan.owner_group_id && ['active', 'paused'].includes(candidate.status)) || null;
	}
	function renderPlansPane(body) {
	  $('sidePaneTitle').textContent = 'Plans';
	  const entries = state.plans || [];
	  body.innerHTML = '<div class="plan-view">'
		+ '<div id="planList" class="plan-list custom-scrollbar"></div>'
		+ '</div>';
	  const list = $('planList');
	  if (!entries.length) {
		list.innerHTML = '<div class="plan-empty"><div><strong>No plans yet</strong><span>Switch a resident agent to Plan mode to turn a complex request into tracked execution steps.</span></div></div>';
		return;
	  }
	  entries.forEach(plan => {
		const steps = plan.steps || [];
		const done = steps.filter(step => ['completed', 'skipped'].includes(step.status)).length;
		const reviewing = steps.filter(step => step.status === 'reviewing').length;
		const running = steps.filter(step => step.status === 'in_progress').length;
		const queued = steps.filter(step => !step.status || step.status === 'pending').length;
		const group = (state.groups || []).find(entry => entry.id === plan.owner_group_id);
		const owner = group ? (group.name + ' · ' + compactPlanActor(plan.owner_agent_id)) : compactPlanActor(plan.owner_agent_id);
		const percent = steps.length ? Math.round((done / steps.length) * 100) : 0;
		const stepSummary = [[running, 'running'], [reviewing, 'in review'], [queued, 'queued']].filter(entry => entry[0]).map(entry => entry[0] + ' ' + entry[1]).join(' · ') || 'all resolved';
		const item = document.createElement('article');
		item.className = 'plan-card';
		item.dataset.planStatus = plan.status || 'draft';
		item.innerHTML = '<div class="plan-card-head"><span class="plan-status">' + escapeHTML(planStatusLabel(plan.status)) + '</span><span class="plan-progress-copy">' + done + ' of ' + steps.length + ' complete</span></div>'
		  + '<h3 class="plan-title">' + escapeHTML(plan.title || plan.id) + '</h3>'
		  + (plan.goal ? '<p class="plan-goal" title="' + escapeHTML(plan.goal) + '">' + escapeHTML(plan.goal) + '</p>' : '')
		  + '<div class="plan-context"><span title="' + escapeHTML(plan.owner_agent_id || '') + '">' + escapeHTML(owner) + '</span><span>max ' + escapeHTML(String(plan.max_concurrency || 1)) + ' parallel</span><span>revision ' + escapeHTML(String(plan.revision || 1)) + '</span></div>'
		  + '<div class="plan-progress-track" role="progressbar" aria-label="Plan progress" aria-valuemin="0" aria-valuemax="' + steps.length + '" aria-valuenow="' + done + '"><i style="width:' + percent + '%"></i></div>'
		  + '<div class="plan-steps-head"><span>Execution</span><span>' + stepSummary + '</span></div>'
		  + '<div class="plan-steps"></div>'
		  + '<div class="plan-actions"></div>';
		const stepList = item.querySelector('.plan-steps');
		steps.forEach((step, index) => {
		  const status = step.status || 'pending';
		  const detail = document.createElement('details');
		  detail.className = 'plan-step status-' + status;
		  const dependencies = (step.dependencies || []).length ? '<div class="plan-step-detail-meta">Depends on · ' + escapeHTML(step.dependencies.join(', ')) + '</div>' : '';
		  const criteria = (step.acceptance_criteria || []).length
			? '<div class="plan-criteria"><span>Acceptance</span><ul>' + step.acceptance_criteria.map(criterion => '<li>' + escapeHTML(criterion) + '</li>').join('') + '</ul></div>'
			: '';
		  detail.innerHTML = '<summary><span class="plan-step-marker" aria-hidden="true">' + planStepMarker(status) + '</span><span class="plan-step-copy"><strong>' + escapeHTML(step.title || step.id) + '</strong><small>' + escapeHTML(planStepMeta(step)) + '</small></span><span class="plan-step-tail">' + String(index + 1).padStart(2, '0') + '</span></summary>'
			+ '<div class="plan-step-detail">' + (step.description ? '<p>' + escapeHTML(step.description) + '</p>' : '') + dependencies + criteria + '</div>';
		  stepList.appendChild(detail);
		});
		const actions = item.querySelector('.plan-actions');
		if (plan.status === 'draft') {
		  const submit = document.createElement('button'); submit.className = 'secondary'; submit.textContent = 'Submit for approval';
		  submit.onclick = () => submitPlanFromUI(plan); actions.appendChild(submit);
		}
		if (plan.status === 'awaiting_approval' || plan.status === 'draft' || plan.status === 'paused') {
		  const conflict = conflictingGroupPlan(plan);
		  const activate = document.createElement('button'); activate.textContent = plan.status === 'paused' ? 'Resume plan' : 'Approve & start';
		  if (conflict) {
			activate.disabled = true;
			activate.title = 'This group is already running ' + (conflict.title || conflict.id);
		  }
		  activate.onclick = () => activatePlanFromUI(plan); actions.appendChild(activate);
		}
		list.appendChild(item);
	  });
	}
	async function submitPlanFromUI(plan) {
	  await api('/api/projects/' + state.project.id + '/plans/' + encodeURIComponent(plan.id) + '/submit', { method: 'POST', body: JSON.stringify({ actor_id: 'user', expected_version: plan.version }) });
	  await loadResidentRuntimeState(); state.sidePanel = 'plans'; renderSidePane();
	}
	async function activatePlanFromUI(plan) {
	  try {
		await api('/api/projects/' + state.project.id + '/plans/' + encodeURIComponent(plan.id) + '/activate', { method: 'POST', body: JSON.stringify({ approved_by: 'user', expected_version: plan.version }) });
		await loadResidentRuntimeState(); state.sidePanel = 'plans'; renderSidePane();
	  } catch (err) {
		notify(err.message || String(err), 'error');
		await loadResidentRuntimeState(); state.sidePanel = 'plans'; renderSidePane();
	  }
	}

	function renderArtifactsPane(body) {
	  $('sidePaneTitle').textContent = 'Artifacts';
	  const entries = (state.artifacts || []).filter(item => item.status !== 'superseded');
	  const fileCount = (state.workspaceFiles || []).length;
	  body.innerHTML = '<div class="blackboard-view"><div class="artifact-view-tabs"><button data-artifact-view="registry" class="' + (state.artifactView === 'registry' ? 'active' : '') + '">Registry · ' + entries.length + '</button><button data-artifact-view="files" class="' + (state.artifactView === 'files' ? 'active' : '') + '">Workspace files' + (fileCount ? ' · ' + fileCount : '') + '</button></div><div id="artifactList" class="blackboard-log custom-scrollbar"></div></div>';
	  body.querySelectorAll('[data-artifact-view]').forEach(button => {
		button.onclick = () => setArtifactView(button.dataset.artifactView);
	  });
	  const list = $('artifactList');
	  if (state.artifactView === 'files') {
		if (!state.workspaceFiles.length) {
		  list.innerHTML = '<div class="muted" style="padding:12px">No workspace files for this agent.</div>';
		  return;
		}
		state.workspaceFiles.forEach(file => {
		  const item = document.createElement('button');
		  item.className = 'workspace-file';
		  item.innerHTML = '<strong>' + escapeHTML(file.filename) + '</strong><div class="muted">' + escapeHTML(file.path) + ' · ' + formatBytes(file.size_bytes || 0) + '</div>';
		  item.onclick = () => openWorkspaceFile(file.path);
		  list.appendChild(item);
		});
		return;
	  }
	  if (!entries.length) {
		list.innerHTML = '<div class="muted" style="padding:12px">No Artifacts yet.</div>';
		return;
	  }
	  entries.forEach(artifact => {
		const item = document.createElement('div');
		item.className = 'blackboard-entry';
		item.innerHTML = '<div class="blackboard-entry-head"><span class="blackboard-entry-kind">' + escapeHTML(artifact.kind || 'artifact') + '</span><span class="blackboard-entry-time">r' + escapeHTML(String(artifact.revision || 1)) + '</span></div>'
		  + '<div class="blackboard-entry-summary">' + escapeHTML(artifact.title || artifact.path || artifact.id) + '</div>'
		  + '<div class="blackboard-entry-meta">' + escapeHTML((artifact.status || 'draft') + ' · ' + (artifact.agent_id || 'agent') + ' · ' + (artifact.path || '')) + '</div>'
		  + (artifact.description ? '<div class="blackboard-entry-detail">' + escapeHTML(artifact.description) + '</div>' : '')
		  + '<div class="form-actions" style="margin-top:8px"></div>';
		const actions = item.querySelector('.form-actions');
		if (artifact.previewable) {
		  const preview = document.createElement('button'); preview.className = 'secondary'; preview.textContent = 'Preview';
		  preview.onclick = () => openArtifactPreview(artifact.id); actions.appendChild(preview);
		}
		if (artifact.status === 'draft' && (artifact.agent_id === currentAgentID() || currentAgentID() === 'karoz')) {
		  const submit = document.createElement('button'); submit.className = 'secondary'; submit.textContent = 'Submit review';
		  submit.onclick = () => updateArtifactStatusFromUI(artifact.id, 'reviewing', 'Submitted from Artifact panel', currentAgentID()); actions.appendChild(submit);
		} else if (artifact.status === 'reviewing') {
		  const approve = document.createElement('button'); approve.textContent = 'Approve';
		  approve.onclick = () => updateArtifactStatusFromUI(artifact.id, 'approved', 'Approved by user', 'user'); actions.appendChild(approve);
		  const changes = document.createElement('button'); changes.className = 'secondary'; changes.textContent = 'Request changes';
		  changes.onclick = async () => {
		    const note = await requestConfirmation({
		      title: 'Request changes',
		      message: 'Add a short note so the agent knows what to revise.',
		      confirmLabel: 'Send note',
		      inputLabel: 'Review note',
		      inputPlaceholder: 'What needs to change?'
		    });
		    if (note === null) return;
		    await updateArtifactStatusFromUI(artifact.id, 'draft', note.trim() || 'Changes requested', 'user');
		  };
		  actions.appendChild(changes);
		}
		list.appendChild(item);
	  });
	}
	async function setArtifactView(view) {
	  state.artifactView = view === 'files' ? 'files' : 'registry';
	  if (state.artifactView === 'files') await loadWorkspaceFiles();
	  state.sidePanel = 'artifacts';
	  renderSidePane();
	}
	async function openArtifactPreview(artifactID) {
	  const preview = await api('/api/projects/' + state.project.id + '/artifacts/' + encodeURIComponent(artifactID) + '/preview');
	  state.preview = preview; state.sidePanel = 'preview'; renderSidePane();
	}
	async function updateArtifactStatusFromUI(artifactID, status, note, actorAgentID) {
	  await api('/api/projects/' + state.project.id + '/artifacts/' + encodeURIComponent(artifactID), { method: 'PATCH', body: JSON.stringify({ status, note, actor_agent_id: actorAgentID }) });
	  await loadResidentRuntimeState(); state.sidePanel = 'artifacts'; renderSidePane();
	}
    function renderInboxEntry(entry) {
      const kind = entry.intent || entry.message_type || 'message';
      const summary = entry.subject || entry.body || 'Inbox message';
      const meta = [entry.source_agent_id ? 'from ' + entry.source_agent_id : '', entry.status || 'pending', entry.thread_key || ''].filter(Boolean).join(' · ');
      return '<div class="blackboard-entry-head"><span class="blackboard-entry-kind">' + escapeHTML(kind) + '</span><span class="blackboard-entry-time">' + escapeHTML(compactDate(entry.created_at)) + '</span></div>'
        + '<div class="blackboard-entry-summary">' + escapeHTML(summary) + '</div>'
        + '<div class="blackboard-entry-meta">' + escapeHTML(meta) + '</div>'
        + (entry.body ? '<div class="blackboard-entry-detail">' + escapeHTML(entry.body) + '</div>' : '');
    }
    function renderMemoryEntry(entry) {
      const kind = entry.layer || 'memory';
      const summary = entry.summary || entry.detail || 'Memory entry';
      const meta = [entry.state || 'active', entry.priority ? 'priority ' + entry.priority : '', entry.id || ''].filter(Boolean).join(' · ');
      return '<div class="blackboard-entry-head"><span class="blackboard-entry-kind">' + escapeHTML(kind) + '</span><span class="blackboard-entry-time">' + escapeHTML(compactDate(entry.updated_at || entry.created_at)) + '</span></div>'
        + '<div class="blackboard-entry-summary">' + escapeHTML(summary) + '</div>'
        + '<div class="blackboard-entry-meta">' + escapeHTML(meta) + '</div>'
        + (entry.detail ? '<div class="blackboard-entry-detail">' + escapeHTML(entry.detail) + '</div>' : '');
    }
    function normalizePreviewHTML(content) {
      const style = '<style>html,body{min-width:0;overflow-wrap:anywhere;}body{margin:0;}img,video,canvas,svg{max-width:100%;height:auto;}</style>';
      if (/<head[^>]*>/i.test(content)) return content.replace(/<head([^>]*)>/i, '<head$1>' + style);
      return '<!doctype html><html><head>' + style + '</head><body>' + content + '</body></html>';
    }
    function formatBytes(value) {
      if (!value) return '0 B';
      const units = ['B', 'KB', 'MB', 'GB'];
      let size = value, unit = 0;
      while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
      return (unit === 0 || size >= 10 ? Math.round(size) : size.toFixed(1)) + ' ' + units[unit];
    }
    function renderAgentAttachments() {
      const row = $('agentAttachmentRow');
      const files = state.agentAttachments || [];
      row.hidden = files.length === 0;
      row.innerHTML = '';
      files.forEach((file, index) => {
        const chip = document.createElement('div');
        chip.className = 'attachment-chip';
        chip.innerHTML = '<span title="' + escapeHTML(file.name) + '">' + escapeHTML(file.name) + '</span><small>' + escapeHTML(formatBytes(file.size || 0)) + '</small><button type="button" title="Remove attachment">×</button>';
        chip.querySelector('button').onclick = () => {
          state.agentAttachments.splice(index, 1);
          renderAgentAttachments();
        };
        row.appendChild(chip);
      });
      $('attachAgentFile').classList.toggle('active', files.length > 0);
    }
    function clearAgentAttachments() {
      state.agentAttachments = [];
      $('agentFileInput').value = '';
      renderAgentAttachments();
    }
    function addAgentAttachments(files) {
      const incoming = Array.from(files || []).filter(file => file && file.size >= 0);
      if (!incoming.length) return;
      const existing = state.agentAttachments || [];
      const next = existing.concat(incoming).slice(0, 12);
      state.agentAttachments = next;
      renderAgentAttachments();
    }
    function messagePreviewWithAttachments(message, files) {
      const names = (files || []).map(file => file.name).filter(Boolean);
      if (!names.length) return message;
      const base = message || 'Attached files';
      return base + '\n\nAttachments:\n' + names.map(name => '- ' + name).join('\n');
    }
    async function loadAgentMessages() {
      if (!state.project || !state.agent) return;
      const projectID = state.project.id;
      const agentID = currentAgentID();
      const page = await api('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID) + '/messages?limit=80') || {};
      if (!state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      state.chatMessages = Array.isArray(page.messages) ? page.messages : (Array.isArray(page) ? page : []);
      state.chatHasMore = !!page.has_more;
      state.chatNextBeforeSeq = page.next_before_seq || (state.chatMessages[0] && state.chatMessages[0].seq) || 0;
      renderChatMessages();
      $('agentOutput').scrollTop = $('agentOutput').scrollHeight;
    }
    function scheduleChatRefresh() {
      if (chatRefreshTimer) clearTimeout(chatRefreshTimer);
      chatRefreshTimer = setTimeout(() => {
        chatRefreshTimer = null;
        void refreshActiveAgentChat();
      }, 400);
    }
    // Merge-based chat sync for messages persisted outside this tab's own
    // streaming flow (handoff runs, queued interrupts, scheduler turns).
    // Skips while a local stream owns the DOM; preserves scroll position.
    async function refreshActiveAgentChat() {
      if (!state.project || !state.agent || state.view !== 'agent') return;
      if (state.chatStreaming || state.chatLoadingHistory) return;
      const projectID = state.project.id;
      const agentID = currentAgentID();
      const page = await api('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID) + '/messages?limit=80').catch(() => null);
      if (!page || !state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      if (state.chatStreaming || state.chatLoadingHistory) return;
      const latest = Array.isArray(page.messages) ? page.messages : (Array.isArray(page) ? page : []);
      const wasEmpty = !(state.chatMessages || []).length;
      const byKey = new Map();
      (state.chatMessages || []).forEach(m => byKey.set(m.id || String(m.seq), m));
      let changed = false;
      latest.forEach(m => {
        const key = m.id || String(m.seq);
        const existing = byKey.get(key);
        if (!existing || (existing.body || '') !== (m.body || '') || (existing.intent || '') !== (m.intent || '')) {
          byKey.set(key, m);
          changed = true;
        }
      });
      if (!changed) return;
      state.chatMessages = Array.from(byKey.values()).sort((a, b) => (a.seq || 0) - (b.seq || 0));
      if (wasEmpty) {
        state.chatHasMore = !!page.has_more;
        state.chatNextBeforeSeq = page.next_before_seq || state.chatNextBeforeSeq;
      }
      const box = $('agentOutput');
      const stickToBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
      renderChatMessages();
      if (stickToBottom) box.scrollTop = box.scrollHeight;
    }
    async function loadEarlierAgentMessages() {
      if (!state.project || !state.agent || !state.chatHasMore || state.chatLoadingHistory) return;
      const projectID = state.project.id;
      const agentID = currentAgentID();
      const beforeSeq = state.chatNextBeforeSeq || (state.chatMessages[0] && state.chatMessages[0].seq) || 0;
      if (!beforeSeq) return;
      state.chatLoadingHistory = true;
      const box = $('agentOutput');
      const previousHeight = box.scrollHeight;
      const page = await api('/api/projects/' + projectID + '/agents/' + encodeURIComponent(agentID) + '/messages?limit=80&before_seq=' + encodeURIComponent(beforeSeq)).catch(() => null);
      state.chatLoadingHistory = false;
      if (!page || !state.project || state.project.id !== projectID || currentAgentID() !== agentID) return;
      const older = Array.isArray(page.messages) ? page.messages : [];
      const seen = new Set(state.chatMessages.map(message => message.id || String(message.seq)));
      state.chatMessages = older.filter(message => !seen.has(message.id || String(message.seq))).concat(state.chatMessages);
      state.chatHasMore = !!page.has_more;
      state.chatNextBeforeSeq = page.next_before_seq || (state.chatMessages[0] && state.chatMessages[0].seq) || 0;
      renderChatMessages();
      box.scrollTop = box.scrollHeight - previousHeight;
    }
    function renderChatMessages() {
      const messages = state.chatMessages || [];
      const box = $('agentOutput'); box.innerHTML = '';
      if (state.chatHasMore) {
        const loadMore = document.createElement('button');
        loadMore.type = 'button';
        loadMore.className = 'load-history';
        loadMore.textContent = state.chatLoadingHistory ? 'Loading...' : 'Load earlier messages';
        loadMore.disabled = !!state.chatLoadingHistory;
        loadMore.onclick = loadEarlierAgentMessages;
        box.appendChild(loadMore);
      }
      if (messages.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'chat-empty';
        const projectName = state.project && state.project.name ? state.project.name : 'this project';
        empty.innerHTML = '<div class="empty-mark" aria-hidden="true">α</div>'
          + '<h1>Start with the outcome.</h1>'
          + '<p>Tell ' + escapeHTML(currentAgentLabel()) + ' what should change in <strong>' + escapeHTML(projectName) + '</strong>. Ask a question, shape a plan, or move straight into development.</p>'
          + '<div class="empty-shortcuts"><span><kbd>↵</kbd> send</span><span><kbd>⇧ ↵</kbd> new line</span><span><kbd>$</kbd> skills</span><span><kbd>/</kbd> commands</span></div>';
        box.appendChild(empty);
        return;
      }
      let skipNextChoiceAssistant = false;
      messages.forEach((m, index) => {
        if (m.role === 'tool_call' || m.role === 'tool_result') {
          const choiceRequest = m.role === 'tool_result' ? parseChoiceRequestResult(m.body || '') : null;
          if (choiceRequest) {
            const hasUserAnswer = messages.slice(index + 1).some(next => next.role === 'user');
            const interactive = !hasUserAnswer && choiceRequest.approval_type !== 'resident_bash';
            appendChoiceRequest(choiceRequest, interactive);
            skipNextChoiceAssistant = true;
            return;
          }
          appendToolMessage(m.role, m.intent || 'tool', '', m.body || '', m.role === 'tool_result');
        } else {
          if (skipNextChoiceAssistant && m.role !== 'user') {
            skipNextChoiceAssistant = false;
            return;
          }
          skipNextChoiceAssistant = false;
          appendAgentMessage(m.role === 'user' ? 'you' : currentAgentID(), m.body || '', m.intent || '');
        }
      });
    }
