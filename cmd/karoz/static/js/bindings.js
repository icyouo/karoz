    $('saveSettings').onclick = async () => {
      const payload = {
        projects_root: $('projectsRoot').value,
        extra_projects_roots: settingsExtraRoots()
      };
      const settings = await api('/api/settings', { method: 'PUT', body: JSON.stringify(payload) });
      state.settings = settings;
      $('projectsRoot').value = settings.projects_root;
      renderExtraWorkspaces();
      renderNewProjectWorkspaceHint();
      await loadProjects();
      closeModal('settingsModal');
      notify('Workspace settings saved.', 'success');
    };
    $('addExtraWorkspace').onclick = () => {
      const input = $('extraWorkspaceInput');
      const value = input.value.trim();
      if (!value) return;
      if (!state.settings) state.settings = { projects_root: $('projectsRoot').value, extra_projects_roots: [] };
      if (!Array.isArray(state.settings.extra_projects_roots)) state.settings.extra_projects_roots = [];
      if (!state.settings.extra_projects_roots.includes(value) && value !== $('projectsRoot').value.trim()) {
        state.settings.extra_projects_roots.push(value);
      }
      input.value = '';
      renderExtraWorkspaces();
    };
    $('chooseProjectsRoot').onclick = async () => {
      try {
        const path = await chooseFolder('Choose the main Karoz workspace');
        if (path) $('projectsRoot').value = path;
      } catch (err) {
        if (!String(err.message || err).includes('User canceled')) notify(err.message, 'error');
      }
    };
    $('chooseExtraWorkspace').onclick = async () => {
      try {
        const path = await chooseFolder('Choose an extra workspace');
        if (path) $('extraWorkspaceInput').value = path;
      } catch (err) {
        if (!String(err.message || err).includes('User canceled')) notify(err.message, 'error');
      }
    };
    $('refreshProjects').onclick = loadProjects;
    $('openNewProject').onclick = () => {
      setNewProjectMode(state.newProjectMode || 'create');
      renderNewProjectWorkspaceHint();
      openModal('newProjectModal');
    };
    $('newProjectCreateTab').onclick = () => setNewProjectMode('create');
    $('newProjectImportTab').onclick = () => {
      setNewProjectMode('import');
      syncImportProjectName(false);
    };
    $('importProjectPath').addEventListener('input', () => syncImportProjectName(false));
    $('chooseImportProjectPath').onclick = async () => {
      try {
        const path = await chooseFolder('Choose an existing project to import');
        if (path) {
          $('importProjectPath').value = path;
          syncImportProjectName(true);
        }
      } catch (err) {
        if (!String(err.message || err).includes('User canceled')) notify(err.message, 'error');
      }
    };
    $('openSettings').onclick = () => {
      renderExtraWorkspaces();
      openModal('settingsModal');
    };
    $('agentRuntimeTools').onclick = async (event) => {
      const button = event.target.closest('button[data-side-panel]');
      if (!button) return;
      await openRuntimePanel(button.dataset.sidePanel);
    };
    $('togglePreviewPane').onclick = async () => {
      if (state.sidePanel === 'preview') {
        closeSidePane();
        return;
      }
      await loadWorkspaceFiles();
      const first = state.workspaceFiles.find(file => /\.html?$/i.test(file.filename)) || state.workspaceFiles[0];
      if (first) await openWorkspaceFile(first.path);
    };
    $('closeSidePane').onclick = () => {
      closeSidePane();
    };
    $('openAddAgent').onclick = async () => {
      if (!state.project) return notify('Select a project first.', 'error');
      await loadAgentTemplates();
      await loadAgentTeams();
      setAddAgentMode(state.addMode || 'role');
      renderAgentTemplates();
      renderAgentTeams();
      openModal('addAgentModal');
    };
    $('openNewTask').onclick = () => {
      if (!state.project) return notify('Select a project first.', 'error');
      openModal('newTaskModal');
    };
    $('openManageAgent').onclick = async () => {
      if (!state.agent) return notify('Select an agent first.', 'error');
      const agents = await api('/api/projects/' + state.project.id + '/agents') || [];
      state.agents = agents;
      state.agent = agents.find(agent => agent.id === currentAgentID()) || agents[0] || state.agent;
      state.manageAgent = state.agent;
      state.routes = await api('/api/projects/' + state.project.id + '/agent-routes').catch(() => []);
      renderManageAgentModal();
      openModal('manageAgentModal');
    };
    function renderManageAgentModal() {
      renderManageAgentList();
      renderManageAgentForm();
      renderRouteControls();
    }
    function renderManageAgentList() {
      const box = $('manageAgentList');
      box.innerHTML = '';
      state.agents.forEach(agent => {
        const b = document.createElement('button');
        const label = agent.nickname || agent.display_name || agent.name || agent.id;
        b.className = state.manageAgent && state.manageAgent.id === agent.id ? 'active' : '';
        b.innerHTML = '<strong>' + escapeHTML(label) + '</strong><div class="muted">' + escapeHTML(agent.role || agent.name || '') + '</div>';
        b.onclick = () => {
          state.manageAgent = agent;
          renderManageAgentModal();
        };
        box.appendChild(b);
      });
    }
    function renderManageAgentForm() {
      const agent = state.manageAgent || state.agent || {};
      $('manageAgentNickname').value = agent.nickname || agent.display_name || agent.name || '';
      $('manageAgentPrompt').value = agent.system_prompt || '';
      $('deleteAgent').disabled = !agent.id || agent.id === 'karoz';
      $('deleteAgent').title = agent.id === 'karoz' ? 'Default agent cannot be deleted' : 'Delete agent';
    }
    function renderRouteControls() {
      const options = state.agents.map(agent => '<option value="' + escapeHTML(agent.id) + '">' + escapeHTML(agent.nickname || agent.display_name || agent.name || agent.id) + '</option>').join('');
      $('routeFrom').innerHTML = options;
      $('routeTo').innerHTML = options;
      if (state.manageAgent) $('routeTo').value = state.manageAgent.id;
      renderRoutes();
    }
    function renderRoutes() {
      const box = $('routeList');
      box.innerHTML = '';
      const routes = state.routes || [];
      if (!routes.length) {
        const empty = document.createElement('div');
        empty.className = 'muted';
        empty.textContent = 'No explicit scope. send_to is open until routes are saved.';
        box.appendChild(empty);
        return;
      }
      routes.forEach((route, index) => {
        const from = state.agents.find(agent => agent.id === route.from_agent_id);
        const to = state.agents.find(agent => agent.id === route.to_agent_id);
        const row = document.createElement('div');
        row.className = 'route-row';
        row.innerHTML = '<div class="muted">' + escapeHTML(from ? (from.nickname || from.display_name || from.name) : route.from_agent_id) + '</div><div class="muted">→ ' + escapeHTML(to ? (to.nickname || to.display_name || to.name) : route.to_agent_id) + '</div><div class="muted">' + escapeHTML(route.intent || 'request') + '</div><button class="secondary icon" title="Remove route">×</button>';
        row.querySelector('button').onclick = () => {
          state.routes.splice(index, 1);
          renderRoutes();
        };
        box.appendChild(row);
      });
    }
    document.querySelectorAll('[data-close-modal]').forEach(button => {
      button.onclick = () => closeModal(button.dataset.closeModal);
    });
    document.querySelectorAll('.modal-backdrop').forEach(backdrop => {
      backdrop.onclick = (event) => {
        if (event.target === backdrop) closeModal(backdrop.id);
      };
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        document.querySelectorAll('.modal-backdrop.open').forEach(modal => closeModal(modal.id));
      }
    });
    $('agentTab').onclick = () => { void switchViewAndRefresh('agent', { push: true }); };
    $('taskTab').onclick = () => { void switchViewAndRefresh('task', { push: true }); };
    $('addRoleTab').onclick = () => setAddAgentMode('role');
    $('addTeamTab').onclick = () => setAddAgentMode('team');
    function setAddAgentMode(mode) {
      state.addMode = mode === 'team' ? 'team' : 'role';
      $('addRoleTab').classList.toggle('active', state.addMode === 'role');
      $('addTeamTab').classList.toggle('active', state.addMode === 'team');
      $('addRoleTab').setAttribute('aria-selected', String(state.addMode === 'role'));
      $('addTeamTab').setAttribute('aria-selected', String(state.addMode === 'team'));
      $('agentTemplateList').hidden = state.addMode !== 'role';
      $('agentTeamList').hidden = state.addMode !== 'team';
      $('roleCreateFields').hidden = state.addMode !== 'role';
      $('teamCreateFields').hidden = state.addMode !== 'team';
    }
    document.querySelectorAll('.chat-mode').forEach(button => {
      button.onclick = () => saveAgentChatType(button.dataset.chatType || 'ask');
    });
    async function loadAgentTemplates() {
      if (state.templates.length) return;
      state.templates = await api('/api/agent-templates') || [];
      state.selectedTemplate = state.templates[0] || null;
    }
    async function loadAgentTeams() {
      if (state.teams.length) return;
      state.teams = await api('/api/agent-team-templates') || [];
      state.selectedTeam = state.teams[0] || null;
    }
    function renderAgentTemplates() {
      const box = $('agentTemplateList');
      box.innerHTML = '';
      state.templates.forEach(template => {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'template-option' + (state.selectedTemplate && state.selectedTemplate.id === template.id ? ' active' : '');
        b.innerHTML = '<strong>' + escapeHTML(template.display_name || template.name) + '</strong><span>' + escapeHTML(template.summary || template.role || '') + '</span>';
        b.onclick = () => {
          state.selectedTemplate = template;
          $('newAgentNickname').value = template.display_name || template.name || '';
          renderAgentTemplates();
        };
        box.appendChild(b);
      });
      if (state.selectedTemplate && !$('newAgentNickname').value.trim()) $('newAgentNickname').value = state.selectedTemplate.display_name || state.selectedTemplate.name || '';
    }
    function renderAgentTeams() {
      const box = $('agentTeamList');
      box.innerHTML = '';
      state.teams.forEach(team => {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'template-option team-option' + (state.selectedTeam && state.selectedTeam.id === team.id ? ' active' : '');
        const members = (team.agents || []).map(agent => agent.nickname || agent.id).join(' → ');
        const edgeText = (team.edges || []).map(edge => edge.from + '→' + edge.to + ':' + edge.kind).join(' · ');
        b.innerHTML = '<strong>' + escapeHTML(team.name || team.id) + '</strong><span>' + escapeHTML(team.description || '') + '</span><div class="team-flow">' + escapeHTML(members) + '</div><div class="team-flow">' + escapeHTML(edgeText) + '</div>';
        b.onclick = () => {
          state.selectedTeam = team;
          $('newTeamInstance').value = team.id || '';
          renderAgentTeams();
        };
        box.appendChild(b);
      });
      if (state.selectedTeam && !$('newTeamInstance').value.trim()) $('newTeamInstance').value = state.selectedTeam.id || '';
    }
    $('createProject').onclick = async () => {
      const name = $('newProjectName').value.trim();
      if (!name) return fieldError('newProjectName', 'Enter a project name.');
      const payload = { name };
      if (state.newProjectMode === 'import') {
        payload.mode = 'import';
        payload.path = $('importProjectPath').value.trim();
        if (!payload.path) return fieldError('importProjectPath', 'Choose a project path.');
      }
      const project = await api('/api/projects', { method: 'POST', body: JSON.stringify(payload) });
      $('newProjectName').value = '';
      $('importProjectPath').value = '';
      setNewProjectMode('create');
      closeModal('newProjectModal');
      await loadSettings();
      await loadProjects();
      await selectProject(project);
      notify('Project ready.', 'success');
    };
    $('sendAgent').onclick = () => sendAgentMessage();
