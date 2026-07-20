    const state = { settings: null, providers: [], projects: [], project: null, agents: [], agent: null, manageAgent: null, templates: [], teams: [], groups: [], plans: [], selectedTemplate: null, selectedTeam: null, addMode: 'role', newProjectMode: 'create', routes: [], task: null, taskLogTab: 'runtime', chatType: 'ask', view: 'agent', inbox: [], memory: [], blackboard: [], artifacts: [], artifactView: 'registry', archive: [], workspaceFiles: [], preview: null, sidePanel: null, agentWorkingById: {}, chatMessages: [], chatHasMore: false, chatNextBeforeSeq: 0, chatLoadingHistory: false, chatStreaming: false, agentAttachments: [], skills: [], skillsProjectID: '', skillSuggest: { open: false, items: [], active: -1, trigger: null } };
    let taskPollTimer = null;
    let agentPollTimer = null;
    let runtimeStateRefreshTimer = null;
    let runtimeEvents = null;
    let runtimeEventsProjectID = '';
    let chatRefreshTimer = null;
    let lastRouteHash = null;
    let applyingRoute = false;
    let projectSelectionSeq = 0;
    let chatModeSaveSerial = 0;
    let modelSettingsSaveSerial = 0;
    let pendingConfirmation = null;
    const $ = (id) => document.getElementById(id);
    function notify(message, kind = 'error') {
      const region = $('toastRegion');
      if (!region || !message) return;
      const toast = document.createElement('div');
      toast.className = 'toast ' + kind;
      toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
      toast.textContent = String(message);
      region.appendChild(toast);
      window.setTimeout(() => toast.remove(), 4200);
    }
    function fieldError(id, message) {
      const field = $(id);
      if (field) {
        field.setAttribute('aria-invalid', 'true');
        field.focus();
        field.addEventListener('input', () => field.removeAttribute('aria-invalid'), { once: true });
      }
      notify(message, 'error');
    }
    function requestConfirmation({ title, message, confirmLabel = 'Confirm', danger = false, inputLabel = '', inputValue = '', inputPlaceholder = '' }) {
      if (pendingConfirmation) pendingConfirmation(null);
      $('confirmTitle').textContent = title || 'Confirm action';
      $('confirmMessage').textContent = message || '';
      $('confirmInputField').hidden = !inputLabel;
      $('confirmInputLabel').textContent = inputLabel || 'Note';
      $('confirmInput').value = inputValue;
      $('confirmInput').placeholder = inputPlaceholder;
      $('acceptConfirm').textContent = confirmLabel;
      $('acceptConfirm').className = danger ? 'danger' : '';
      openModal('confirmModal');
      if (inputLabel) window.setTimeout(() => $('confirmInput').focus(), 0);
      else window.setTimeout(() => $('acceptConfirm').focus(), 0);
      return new Promise(resolve => { pendingConfirmation = resolve; });
    }
    function finishConfirmation(accepted) {
      const resolve = pendingConfirmation;
      pendingConfirmation = null;
      const value = $('confirmInputField').hidden ? true : $('confirmInput').value;
      $('confirmModal').classList.remove('open');
      if (resolve) resolve(accepted ? value : null);
    }
    function openModal(id) {
      $(id).classList.add('open');
      const focusable = Array.from($(id).querySelectorAll('input:not([type="hidden"]), textarea, select, button:not([disabled])'))
        .find(element => !element.closest('[hidden]'));
      if (focusable) setTimeout(() => focusable.focus(), 0);
    }
    function closeModal(id) {
      $(id).classList.remove('open');
      if (id === 'confirmModal' && pendingConfirmation) {
        const resolve = pendingConfirmation;
        pendingConfirmation = null;
        resolve(null);
      }
    }
    $('cancelConfirm').onclick = () => finishConfirmation(false);
    $('acceptConfirm').onclick = () => finishConfirmation(true);
    async function api(path, opts = {}) {
      const res = await fetch(path, { headers: { 'content-type': 'application/json' }, ...opts });
      if (!res.ok) throw new Error(await res.text());
      const type = res.headers.get('content-type') || '';
      return type.includes('application/json') ? res.json() : res.text();
    }
    async function chooseFolder(prompt) {
      const res = await api('/api/folder-dialog', { method: 'POST', body: JSON.stringify({ prompt }) });
      return res && res.path ? res.path : '';
    }
    async function loadSettings() {
      const settings = await api('/api/settings');
      state.settings = settings;
      $('projectsRoot').value = settings.projects_root;
      renderExtraWorkspaces();
      renderNewProjectWorkspaceHint();
    }
    async function loadResidentProviders() {
      const payload = await api('/api/runtime/providers');
      state.providers = payload && Array.isArray(payload.providers) ? payload.providers : [];
      renderModelCatalog();
    }
    function selectedModelDescriptor() {
      const value = $('agentModel').value;
      for (const provider of state.providers || []) {
        const model = (provider.models || []).find(item => item.id === value && item.provider === provider.id);
        if (model) return { provider, model };
      }
      return null;
    }
    function renderModelCatalog() {
      const select = $('agentModel');
      if (!select) return;
      select.innerHTML = '';
      (state.providers || []).forEach(provider => {
        const group = document.createElement('optgroup');
        group.label = provider.display_name + (provider.available ? '' : ' - unavailable');
        group.disabled = !provider.available;
        group.title = provider.reason || '';
        (provider.models || []).forEach(model => {
          const option = document.createElement('option');
          option.value = model.id;
          option.dataset.provider = provider.id;
          option.textContent = model.display_name || model.id;
          group.appendChild(option);
        });
        select.appendChild(group);
      });
      restoreAgentModelSettings();
    }
    function settingsExtraRoots() {
      return state.settings && Array.isArray(state.settings.extra_projects_roots) ? state.settings.extra_projects_roots : [];
    }
    function renderExtraWorkspaces() {
      const box = $('extraWorkspaceList');
      if (!box) return;
      box.innerHTML = '';
      const roots = settingsExtraRoots();
      if (!roots.length) {
        const empty = document.createElement('div');
        empty.className = 'field-note';
        empty.textContent = 'No extra workspaces added.';
        box.appendChild(empty);
        return;
      }
      roots.forEach((root, index) => {
        const row = document.createElement('div');
        row.className = 'workspace-root-row';
        row.innerHTML = '<code title="' + escapeHTML(root) + '">' + escapeHTML(root) + '</code><button type="button" class="secondary icon" title="Remove workspace">×</button>';
        row.querySelector('button').onclick = () => {
          state.settings.extra_projects_roots.splice(index, 1);
          renderExtraWorkspaces();
        };
        box.appendChild(row);
      });
    }
    function renderNewProjectWorkspaceHint() {
      const el = $('newProjectWorkspaceHint');
      if (!el) return;
      const root = state.settings && state.settings.projects_root ? state.settings.projects_root : '';
      if (state.newProjectMode === 'import') {
        el.textContent = 'Display name for this imported project. The folder is not renamed.';
      } else {
        el.textContent = root ? 'Main workspace: ' + root : '';
      }
    }
    function setNewProjectMode(mode) {
      state.newProjectMode = mode === 'import' ? 'import' : 'create';
      $('newProjectCreateTab').classList.toggle('active', state.newProjectMode === 'create');
      $('newProjectImportTab').classList.toggle('active', state.newProjectMode === 'import');
      $('newProjectCreateTab').setAttribute('aria-selected', String(state.newProjectMode === 'create'));
      $('newProjectImportTab').setAttribute('aria-selected', String(state.newProjectMode === 'import'));
      $('importProjectPathField').hidden = state.newProjectMode !== 'import';
      $('createProject').textContent = state.newProjectMode === 'import' ? 'Import Project' : 'Create Project';
      renderNewProjectWorkspaceHint();
    }
    function pathBaseName(path) {
      return String(path || '').trim().replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '';
    }
    function syncImportProjectName(force = false) {
      if (state.newProjectMode !== 'import') return;
      const name = pathBaseName($('importProjectPath').value);
      if (name && (force || !$('newProjectName').value.trim())) $('newProjectName').value = name;
    }
    function switchView(view) {
      state.view = view;
      $('agentTab').classList.toggle('active', view === 'agent');
      $('taskTab').classList.toggle('active', view === 'task');
      $('agentTab').setAttribute('aria-selected', String(view === 'agent'));
      $('taskTab').setAttribute('aria-selected', String(view === 'task'));
      $('agentList').style.display = view === 'agent' ? '' : 'none';
      $('tasks').style.display = view === 'task' ? '' : 'none';
      $('agentActions').hidden = view !== 'agent';
      $('taskActions').hidden = view !== 'task';
      $('agentView').classList.toggle('active', view === 'agent');
      $('taskView').classList.toggle('active', view === 'task');
    }
    async function switchViewAndRefresh(view, opts = {}) {
      switchView(view);
      syncRouteHash(Boolean(opts.push));
      if (!state.project) return;
      try {
        if (view === 'agent') {
          await refreshAgentStates();
          scheduleChatRefresh();
        } else {
          await loadTasks();
        }
      } catch (err) {
        notify('Could not refresh the ' + view + ' list: ' + err.message, 'error');
      }
    }
