    const composerDropTarget = document.querySelector('.chat-composer-box');
    ['dragenter', 'dragover'].forEach(type => {
      composerDropTarget.addEventListener(type, (event) => {
        if (!event.dataTransfer || !Array.from(event.dataTransfer.types || []).includes('Files')) return;
        event.preventDefault();
        event.dataTransfer.dropEffect = 'copy';
        composerDropTarget.classList.add('drag-over');
      });
    });
    ['dragleave', 'drop'].forEach(type => {
      composerDropTarget.addEventListener(type, (event) => {
        if (type === 'drop') {
          event.preventDefault();
          addAgentAttachments(event.dataTransfer ? event.dataTransfer.files : []);
        }
        composerDropTarget.classList.remove('drag-over');
      });
    });
    ['dragover', 'drop'].forEach(type => {
      window.addEventListener(type, (event) => {
        if (event.dataTransfer && Array.from(event.dataTransfer.types || []).includes('Files')) event.preventDefault();
      });
    });
    $('agentMessage').addEventListener('keydown', (event) => {
      if (state.skillSuggest.open) {
        if (event.key === 'ArrowUp') {
          event.preventDefault();
          moveSkillSuggest(-1);
          return;
        }
        if (event.key === 'ArrowDown') {
          event.preventDefault();
          moveSkillSuggest(1);
          return;
        }
        if ((event.key === 'Enter' || event.key === 'Tab') && state.skillSuggest.active >= 0) {
          event.preventDefault();
          chooseSkillSuggest(state.skillSuggest.active);
          return;
        }
        if (event.key === 'Escape') {
          event.preventDefault();
          closeSkillSuggest();
          return;
        }
      } else if (event.key === 'ArrowUp' && currentSkillTrigger($('agentMessage'))) {
        event.preventDefault();
        updateSkillSuggest().then(() => moveSkillSuggest(-1));
        return;
      }
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
        event.preventDefault();
        $('sendAgent').click();
      }
    });
    $('createTask').onclick = async () => {
      if (!state.project) return notify('Select a project first.', 'error');
      const t = await api('/api/projects/' + state.project.id + '/tasks', { method: 'POST', body: JSON.stringify({ type: $('taskType').value, title: $('taskTitle').value, goal: $('taskGoal').value }) });
      $('taskTitle').value = '';
      $('taskGoal').value = '';
      closeModal('newTaskModal');
      await loadTasks(); await selectTask(t);
      notify('Task created.', 'success');
    };
    $('runTask').onclick = async () => {
      if (!state.project || !state.task) return;
      const t = await api('/api/projects/' + state.project.id + '/tasks/' + state.task.id + '/run', { method: 'POST' });
      state.task = t;
      switchView('task');
      await loadTasks();
      await renderTaskDetail();
      syncTaskPolling();
    };
    document.querySelectorAll('.task-log-tab').forEach(button => {
      button.onclick = async () => {
        state.taskLogTab = button.dataset.taskLog || 'runtime';
        await loadTaskLog();
      };
    });
    $('createAgent').onclick = async () => {
      if (!state.project || !state.selectedTemplate) return;
      const nickname = $('newAgentNickname').value.trim() || state.selectedTemplate.display_name || state.selectedTemplate.name;
      const agent = await api('/api/projects/' + state.project.id + '/agents', { method: 'POST', body: JSON.stringify({ template_id: state.selectedTemplate.id, nickname }) });
      closeModal('addAgentModal');
      $('newAgentNickname').value = '';
      state.agent = agent;
      await loadAgents();
      await selectAgent(agent);
      notify('Agent added.', 'success');
    };
    $('createTeam').onclick = async () => {
      if (!state.project || !state.selectedTeam) return;
      const instance = $('newTeamInstance').value.trim() || state.selectedTeam.id;
      const resp = await api('/api/projects/' + state.project.id + '/agent-teams', { method: 'POST', body: JSON.stringify({ template_id: state.selectedTeam.id, instance }) });
      closeModal('addAgentModal');
      $('newTeamInstance').value = '';
      await loadAgents();
      state.routes = resp.routes || state.routes;
      if (resp.agents && resp.agents[0]) await selectAgent(resp.agents[0]);
      notify('Agent team added.', 'success');
    };
    $('saveAgentConfig').onclick = async () => {
      if (!state.project || !state.manageAgent) return;
      const updated = await api('/api/projects/' + state.project.id + '/agents/' + encodeURIComponent(state.manageAgent.id), { method: 'PATCH', body: JSON.stringify({ nickname: $('manageAgentNickname').value, system_prompt: $('manageAgentPrompt').value }) });
      state.manageAgent = updated;
      if (state.agent && state.agent.id === updated.id) state.agent = updated;
      await loadAgents();
      renderManageAgentModal();
      updateAgentChrome();
      notify('Agent settings saved.', 'success');
    };
    $('deleteAgent').onclick = async () => {
      if (!state.project || !state.manageAgent) return;
      if (state.manageAgent.id === 'karoz') return notify('The default agent cannot be deleted.', 'error');
      const label = state.manageAgent.nickname || state.manageAgent.display_name || state.manageAgent.name || state.manageAgent.id;
      const confirmed = await requestConfirmation({
        title: 'Delete ' + label + '?',
        message: 'This removes the agent and its local configuration from the project. This action cannot be undone.',
        confirmLabel: 'Delete agent',
        danger: true
      });
      if (!confirmed) return;
      const deletedID = state.manageAgent.id;
      await api('/api/projects/' + state.project.id + '/agents/' + encodeURIComponent(deletedID), { method: 'DELETE' });
      await loadAgents();
      state.manageAgent = state.agents.find(agent => agent.id !== deletedID) || state.agents[0] || null;
      if (!state.agent || state.agent.id === deletedID) {
        state.agent = state.manageAgent;
        if (state.agent) await selectAgent(state.agent);
      }
      state.routes = await api('/api/projects/' + state.project.id + '/agent-routes').catch(() => []);
      renderManageAgentModal();
      updateAgentChrome();
      notify('Agent deleted.', 'success');
    };
    $('addRoute').onclick = () => {
      const from = $('routeFrom').value;
      const to = $('routeTo').value;
      const intent = $('routeIntent').value || 'request';
      if (!from || !to || from === to) return notify('Choose two different agents.', 'error');
      if (!state.routes.some(route => route.from_agent_id === from && route.to_agent_id === to && (route.intent || 'request') === intent)) {
        state.routes.push({ from_agent_id: from, to_agent_id: to, intent, enabled: true });
      }
      renderRoutes();
    };
    $('saveAgentRoutes').onclick = async () => {
      if (!state.project) return;
      state.routes = await api('/api/projects/' + state.project.id + '/agent-routes', { method: 'PUT', body: JSON.stringify({ routes: state.routes }) });
      renderRoutes();
      notify('Communication scope saved.', 'success');
    };
    window.addEventListener('popstate', () => { void applyRoute(); });
    window.addEventListener('hashchange', () => { void applyRoute(); });
    (async function init(){ await Promise.all([loadSettings(), loadResidentProviders()]); await loadProjects(); await applyRoute(); })().catch(err => notify(err.message, 'error'));
