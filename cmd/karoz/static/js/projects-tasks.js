    async function loadProjects() {
      state.projects = await api('/api/projects');
      renderProjects();
      if (!state.project && state.projects[0] && !parseHashRoute().projectID) await selectProject(state.projects[0]);
    }
    function renderProjects() {
      const box = $('projects'); box.innerHTML = '';
      state.projects.forEach(p => {
        const b = document.createElement('button');
        b.className = 'project' + (state.project && state.project.id === p.id ? ' active' : '');
        const initial = (p.name || '?').trim().slice(0, 1).toUpperCase();
        b.innerHTML = '<span class="initial">' + initial + '</span><span class="project-text"><strong>' + escapeHTML(p.name) + '</strong><div class="muted">' + escapeHTML((p.workspace_type || 'main') + ' · ' + p.default_branch) + '</div></span>';
        b.onclick = () => selectProject(p, { push: true });
        box.appendChild(b);
      });
    }
    async function selectProject(p, opts = {}) {
      if (!p) return;
      if (state.project && state.project.id === p.id && state.agent) {
        switchView(state.view);
        return;
      }
      const seq = ++projectSelectionSeq;
      stopRuntimeSubscriptions();
      state.project = p; state.task = null; state.agent = null;
      state.agents = [];
      state.inbox = [];
      state.memory = [];
      state.blackboard = [];
      state.archive = [];
      state.workspaceFiles = [];
      state.chatMessages = [];
      state.chatHasMore = false;
      state.chatNextBeforeSeq = 0;
      state.preview = null;
      state.sidePanel = null;
      state.skills = [];
      state.skillsProjectID = '';
      closeSkillSuggest();
      closeSidePane();
      renderProjects();
      renderAgents();
      $('agentOutput').innerHTML = '';
      $('tasks').innerHTML = '';
      $('projectMeta').textContent = projectWorkspaceLabel(p) + ' · branch ' + p.default_branch + ' · agent Karoz';
      await loadAgents();
      if (seq !== projectSelectionSeq || !state.project || state.project.id !== p.id) return;
      await loadProjectSkills();
      if (seq !== projectSelectionSeq || !state.project || state.project.id !== p.id) return;
      updateAgentChrome();
      await loadTasks();
      if (seq !== projectSelectionSeq || !state.project || state.project.id !== p.id) return;
      syncAgentPolling();
      syncRuntimeEvents();
      switchView(state.view);
      syncRouteHash(Boolean(opts.push));
    }
    function projectWorkspaceLabel(project) {
      const kind = project && project.workspace_type ? project.workspace_type : 'main';
      const root = project && project.workspace_root ? project.workspace_root : '';
      return root ? kind + ' workspace ' + root : kind + ' workspace';
    }
    async function loadTasks() {
      if (!state.project) return;
      const projectID = state.project.id;
      const tasks = await api('/api/projects/' + projectID + '/tasks') || [];
      if (!state.project || state.project.id !== projectID) return;
      const box = $('tasks'); box.innerHTML = '';
      if (tasks.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'muted';
        empty.style.padding = '10px';
        empty.textContent = 'No tasks yet.';
        box.appendChild(empty);
        return;
      }
      tasks.forEach(t => {
        const b = document.createElement('button');
        b.className = 'nav-item' + (state.task && state.task.id === t.id ? ' active' : '');
        const statusClass = String(t.status || 'pending').replace(/[^a-zA-Z0-9_-]/g, '_');
        b.innerHTML = '<strong>' + escapeHTML(t.title || 'Untitled task') + '</strong><div class="nav-meta"><span>' + escapeHTML(taskCategoryLabel(t.type) + ' · ' + taskTypeLabel(t.type)) + '</span><span class="status-pill ' + statusClass + '">' + escapeHTML(t.status || 'pending') + '</span></div>';
        b.onclick = () => selectTask(t, { push: true });
        box.appendChild(b);
      });
    }
    async function selectTask(t, opts = {}) {
      state.task = await api('/api/projects/' + state.project.id + '/tasks/' + t.id);
      await loadTasks();
      switchView('task');
      syncRouteHash(Boolean(opts.push));
      await renderTaskDetail();
      syncTaskPolling();
    }
    async function renderTaskDetail() {
      const task = state.task;
      $('taskEmpty').hidden = !!task;
      $('taskDetail').hidden = !task;
      if (!task) return;
      const statusClass = String(task.status || 'pending').replace(/[^a-zA-Z0-9_-]/g, '_');
      $('taskTitleText').textContent = task.title || 'Untitled task';
      $('taskMetaText').textContent = taskCategoryLabel(task.type) + ' · ' + taskTypeLabel(task.type) + ' · ' + compactDate(task.created_at);
      $('taskStatusText').className = 'status-pill ' + statusClass;
      $('taskStatusText').textContent = task.status || 'pending';
      $('taskGoalText').textContent = task.goal || task.description || '-';
      $('taskResultText').textContent = task.failure_summary || task.result || '-';
      $('runTask').disabled = ['running', 'verifying', 'deploying', 'merging'].includes(task.status || '');
      renderTaskFields(task);
      await loadTaskLog();
      syncTaskPolling();
    }
    function isLiveTask(task) {
      const status = String(task && task.status || '').toLowerCase();
      return ['running', 'verifying', 'deploying', 'merging'].includes(status);
    }
    function syncTaskPolling() {
      if (taskPollTimer) {
        clearInterval(taskPollTimer);
        taskPollTimer = null;
      }
      if (!state.project || !state.task || !isLiveTask(state.task)) return;
      taskPollTimer = setInterval(refreshSelectedTask, 1200);
    }
    async function refreshSelectedTask() {
      if (!state.project || !state.task) {
        syncTaskPolling();
        return;
      }
      try {
        state.task = await api('/api/projects/' + state.project.id + '/tasks/' + state.task.id);
        await loadTasks();
        await renderTaskDetail();
      } catch (err) {
        console.error(err);
      }
    }
    function renderTaskFields(task) {
      const fields = [
        ['Base branch', task.base_branch || 'main'],
        ['Task branch', task.task_branch || '-'],
        ['Worktree', task.worktree_path || '-'],
        ['Commit', task.commit_sha || '-'],
        ['Merged at', task.merged_at ? compactDate(task.merged_at) : '-'],
        ['Updated', compactDate(task.updated_at)]
      ];
      if (task.type === 'deploy' || task.type === 'deployment') {
        fields.splice(1, 0, ['Deploy mode', 'local command']);
        fields.splice(3, 0, ['Deploy status', task.status || 'pending']);
      }
      $('taskFieldGrid').innerHTML = fields.map(([label, value]) => '<div class="task-field"><label>' + escapeHTML(label) + '</label><span title="' + escapeHTML(value) + '">' + escapeHTML(value) + '</span></div>').join('');
    }
    async function loadTaskLog() {
      if (!state.project || !state.task) return;
      const tab = state.taskLogTab || 'runtime';
      document.querySelectorAll('.task-log-tab').forEach(button => button.classList.toggle('active', button.dataset.taskLog === tab));
      if (tab === 'all') {
        $('taskLog').textContent = await api('/api/projects/' + state.project.id + '/tasks/' + state.task.id + '/logs');
        return;
      }
      const path = tab === 'deploy' ? 'deployment-log' : 'runtime-log';
      const payload = await api('/api/projects/' + state.project.id + '/tasks/' + state.task.id + '/' + path);
      $('taskLog').textContent = payload.content || '';
    }
    function taskTypeLabel(type) {
      if (type === 'bug' || type === 'bugfix') return 'Bugfix';
      if (type === 'deploy' || type === 'deployment') return 'Deploy';
      return 'Feature';
    }
    function taskCategoryLabel(type) {
      return type === 'deploy' || type === 'deployment' ? 'Deployment' : 'Development';
    }
    function compactDate(value) {
      if (!value) return '-';
      const date = new Date(value);
      if (Number.isNaN(date.getTime())) return '-';
      return date.toLocaleString();
    }
