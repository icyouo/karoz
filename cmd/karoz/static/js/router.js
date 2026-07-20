    // Hash router: deep-linkable views + browser back/forward.
    // Routes: #/ | #/p/<projectID> | #/p/<projectID>/agent/<agentID> |
    //         #/p/<projectID>/tasks | #/p/<projectID>/task/<taskID>
    function parseHashRoute() {
      const raw = (location.hash || '').replace(/^#\/?/, '');
      const parts = raw.split('/').filter(Boolean).map(part => decodeURIComponent(part));
      const route = { projectID: '', view: 'agent', agentID: '', taskID: '' };
      if (parts[0] !== 'p' || !parts[1]) return route;
      route.projectID = parts[1];
      if (parts[2] === 'task' && parts[3]) { route.view = 'task'; route.taskID = parts[3]; }
      else if (parts[2] === 'tasks') { route.view = 'task'; }
      else if (parts[2] === 'agent' && parts[3]) { route.view = 'agent'; route.agentID = parts[3]; }
      return route;
    }
    function hashForState() {
      if (!state.project) return '#/';
      let hash = '#/p/' + encodeURIComponent(state.project.id);
      if (state.view === 'task') hash += state.task ? '/task/' + encodeURIComponent(state.task.id) : '/tasks';
      else if (state.agent) hash += '/agent/' + encodeURIComponent(state.agent.id);
      return hash;
    }
    function syncRouteHash(push) {
      if (applyingRoute) return;
      const hash = hashForState();
      if ((location.hash || '#/') === hash) return;
      lastRouteHash = hash;
      if (push) history.pushState(null, '', hash);
      else history.replaceState(null, '', hash);
    }
    async function applyRoute() {
      const hash = location.hash || '#/';
      if (hash === lastRouteHash && state.project) return;
      lastRouteHash = hash;
      const route = parseHashRoute();
      applyingRoute = true;
      try {
        if (!state.projects.length) return;
        const project = state.projects.find(p => p.id === route.projectID) || state.projects[0];
        if (!project) return;
        if (!state.project || state.project.id !== project.id) await selectProject(project);
        if (!state.project || state.project.id !== project.id) return;
        if (route.view === 'task') {
          if (route.taskID) {
            try {
              await selectTask({ id: route.taskID });
            } catch (err) {
              notify('Task not found: ' + err.message, 'error');
              state.task = null;
              switchView('task');
              await loadTasks();
              await renderTaskDetail();
              syncTaskPolling();
            }
          } else {
            state.task = null;
            switchView('task');
            await loadTasks();
            await renderTaskDetail();
            syncTaskPolling();
          }
        } else {
          if (route.agentID) {
            const agent = state.agents.find(item => item.id === route.agentID);
            if (agent) await selectAgent(agent);
          }
          switchView('agent');
        }
      } finally {
        applyingRoute = false;
        syncRouteHash(false);
      }
    }
