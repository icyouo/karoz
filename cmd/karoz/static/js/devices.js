(() => {
  let activeDeviceSocket = null;
  let activeDeviceSocketID = '';
  let activeVideoDecoder = null;
  let activeImageObjectURL = '';
  let deviceKeyFrameSeen = false;

  function isPNGFrame(bytes) {
    if (!bytes || bytes.length < 8) return false;
    return bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e && bytes[3] === 0x47 &&
      bytes[4] === 0x0d && bytes[5] === 0x0a && bytes[6] === 0x1a && bytes[7] === 0x0a;
  }

  function base64ToUint8Array(b64) {
    if (!b64) return new Uint8Array(0);
    if (typeof Buffer !== 'undefined' && typeof atob === 'undefined') {
      return new Uint8Array(Buffer.from(b64, 'base64'));
    }
    const bin = atob(b64);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function hasAnnexBKeyFrame(bytes) {
    if (!bytes || bytes.length < 4) return false;
    for (let i = 0; i < bytes.length - 3; i++) {
      let headerIdx = -1;
      if (bytes[i] === 0 && bytes[i + 1] === 0) {
        if (bytes[i + 2] === 1) {
          headerIdx = i + 3;
          i += 2;
        } else if (i + 4 < bytes.length && bytes[i + 2] === 0 && bytes[i + 3] === 1) {
          headerIdx = i + 4;
          i += 3;
        }
      }
      if (headerIdx >= 0 && headerIdx < bytes.length && (bytes[headerIdx] & 0x1f) === 5) {
        return true;
      }
    }
    return false;
  }

  function computeContainedTouchCoords(clientX, clientY, rect, naturalWidth, naturalHeight) {
    if (!rect || rect.width <= 0 || rect.height <= 0 || !naturalWidth || !naturalHeight) return null;
    const containerAspect = rect.width / rect.height;
    const mediaAspect = naturalWidth / naturalHeight;
    let renderedW = rect.width;
    let renderedH = rect.height;
    let offsetX = 0;
    let offsetY = 0;
    if (containerAspect > mediaAspect) {
      renderedW = rect.height * mediaAspect;
      offsetX = (rect.width - renderedW) / 2;
    } else if (containerAspect < mediaAspect) {
      renderedH = rect.width / mediaAspect;
      offsetY = (rect.height - renderedH) / 2;
    }
    const relX = clientX - rect.left - offsetX;
    const relY = clientY - rect.top - offsetY;
    if (relX < 0 || relY < 0 || relX > renderedW || relY > renderedH) return null;
    const x = Math.max(0, Math.min(naturalWidth - 1, Math.round((relX / renderedW) * naturalWidth)));
    const y = Math.max(0, Math.min(naturalHeight - 1, Math.round((relY / renderedH) * naturalHeight)));
    return { x, y, width: naturalWidth, height: naturalHeight };
  }

  function revokeActiveImageURL() {
    if (activeImageObjectURL && typeof URL !== 'undefined' && URL.revokeObjectURL) {
      URL.revokeObjectURL(activeImageObjectURL);
      activeImageObjectURL = '';
    }
  }

  function closeDeviceStream() {
    if (activeDeviceSocket) {
      try { activeDeviceSocket.close(); } catch (_) {}
      activeDeviceSocket = null;
    }
    activeDeviceSocketID = '';
    if (activeVideoDecoder) {
      try { activeVideoDecoder.close(); } catch (_) {}
      activeVideoDecoder = null;
    }
    deviceKeyFrameSeen = false;
    revokeActiveImageURL();
  }

  function selectedDeviceRecord() {
    const devices = Array.isArray(state.devices) ? state.devices : [];
    if (!devices.length) return null;
    return devices.find(d => d.id === state.selectedDeviceID) || devices[0];
  }

  async function loadDevices() {
    if (!state.project) {
      state.devices = [];
      state.selectedDeviceID = '';
      return [];
    }
    const res = await api('/api/projects/' + encodeURIComponent(state.project.id) + '/devices').catch(() => ({ devices: [] }));
    const list = Array.isArray(res && res.devices) ? res.devices : [];
    state.devices = list;
    if (!list.some(d => d.id === state.selectedDeviceID)) {
      const online = list.find(d => d.status === 'ready' || d.status === 'running') || list[0];
      state.selectedDeviceID = online ? online.id : '';
    }
    return list;
  }

  async function openDevicesPane() {
    if (!state.project) return;
    await loadDevices();
    state.sidePanel = 'devices';
    renderSidePane();
    if (typeof renderRuntimeStrip === 'function') renderRuntimeStrip();
  }

  function updateDeviceStatusText(text) {
    state.deviceStreamStatus = text;
    const pill = document.getElementById('deviceStreamStatus');
    if (pill) pill.textContent = text;
  }

  function renderPNGFrameToSurface(bytes) {
    const img = document.querySelector('#sidePaneBody .device-screen-img');
    const canvas = document.querySelector('#sidePaneBody .device-screen-canvas');
    const empty = document.querySelector('#sidePaneBody .device-screen-empty');
    if (!img) return;
    revokeActiveImageURL();
    const blob = new Blob([bytes], { type: 'image/png' });
    activeImageObjectURL = URL.createObjectURL(blob);
    img.src = activeImageObjectURL;
    img.hidden = false;
    if (canvas) canvas.hidden = true;
    if (empty) empty.hidden = true;
  }

  function configureWebCodecsDecoder(cfg) {
    if (typeof VideoDecoder === 'undefined' || !cfg || !cfg.codec || cfg.codec === 'png') {
      return false;
    }
    const canvas = document.querySelector('#sidePaneBody .device-screen-canvas');
    const img = document.querySelector('#sidePaneBody .device-screen-img');
    const empty = document.querySelector('#sidePaneBody .device-screen-empty');
    if (!canvas) return false;
    if (activeVideoDecoder) {
      try { activeVideoDecoder.close(); } catch (_) {}
      activeVideoDecoder = null;
    }
    deviceKeyFrameSeen = false;
    const ctx = canvas.getContext('2d');
    const descBytes = cfg.format === 'avcc' && cfg.description ? base64ToUint8Array(cfg.description) : undefined;
    try {
      activeVideoDecoder = new VideoDecoder({
        output(frame) {
          try {
            if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
              canvas.width = frame.displayWidth || cfg.width || 1080;
              canvas.height = frame.displayHeight || cfg.height || 1920;
            }
            if (ctx) ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
            canvas.hidden = false;
            if (img) img.hidden = true;
            if (empty) empty.hidden = true;
          } finally {
            frame.close();
          }
        },
        error() {
          if (activeVideoDecoder) {
            try { activeVideoDecoder.close(); } catch (_) {}
            activeVideoDecoder = null;
          }
        }
      });
      const decoderConfig = {
        codec: cfg.codec,
        optimizeForLatency: true,
      };
      if (descBytes && descBytes.length) {
        decoderConfig.description = descBytes;
      }
      activeVideoDecoder.configure(decoderConfig);
      return true;
    } catch (_) {
      activeVideoDecoder = null;
      return false;
    }
  }

  function handleDeviceBinaryFrame(bytes) {
    if (isPNGFrame(bytes)) {
      renderPNGFrameToSurface(bytes);
      return;
    }
    if (!activeVideoDecoder || typeof EncodedVideoChunk === 'undefined') return;
    const isKey = hasAnnexBKeyFrame(bytes);
    if (!deviceKeyFrameSeen && !isKey) return;
    if (isKey) deviceKeyFrameSeen = true;
    try {
      activeVideoDecoder.decode(new EncodedVideoChunk({
        type: isKey ? 'key' : 'delta',
        timestamp: performance.now() * 1000,
        data: bytes,
      }));
    } catch (_) {}
  }

  function connectDeviceStream(dev) {
    if (!state.project || !dev || !dev.id || (dev.status !== 'ready' && dev.status !== 'running')) {
      closeDeviceStream();
      updateDeviceStatusText(dev ? (dev.status || 'offline') : 'no device');
      return;
    }
    if (activeDeviceSocket && activeDeviceSocketID === dev.id && activeDeviceSocket.readyState <= 1) {
      return;
    }
    closeDeviceStream();
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsURL = proto + '//' + location.host + '/api/projects/' + encodeURIComponent(state.project.id) + '/devices/' + encodeURIComponent(dev.id) + '/ws';
    const ws = new WebSocket(wsURL);
    ws.binaryType = 'arraybuffer';
    activeDeviceSocket = ws;
    activeDeviceSocketID = dev.id;
    updateDeviceStatusText('connecting');

    ws.onopen = () => {
      updateDeviceStatusText('live');
    };
    ws.onmessage = (event) => {
      if (typeof event.data === 'string') {
        try {
          const msg = JSON.parse(event.data);
          if (msg.type === 'video_config') {
            if (msg.codec && msg.codec !== 'png') {
              configureWebCodecsDecoder(msg);
            }
            updateDeviceStatusText('live · ' + (msg.codec || 'png'));
          } else if (msg.type === 'error' && msg.error) {
            updateDeviceStatusText(msg.error);
          }
        } catch (_) {}
        return;
      }
      if (event.data instanceof ArrayBuffer) {
        handleDeviceBinaryFrame(new Uint8Array(event.data));
      }
    };
    ws.onclose = () => {
      if (activeDeviceSocket === ws) {
        activeDeviceSocket = null;
        activeDeviceSocketID = '';
        updateDeviceStatusText('disconnected');
      }
    };
    ws.onerror = () => {
      updateDeviceStatusText('stream error');
    };
  }

  async function sendDeviceAction(dev, action) {
    if (!state.project || !dev) return;
    await api('/api/projects/' + encodeURIComponent(state.project.id) + '/devices/' + encodeURIComponent(dev.id) + '/actions', {
      method: 'POST',
      body: JSON.stringify(action),
    });
  }

  async function renderDevicesPane(body) {
    $('sidePaneTitle').textContent = 'Devices';
    const devices = Array.isArray(state.devices) ? state.devices : [];
    const dev = selectedDeviceRecord();
    const options = devices.length
      ? devices.map(d => '<option value="' + escapeHTML(d.id) + '"' + (dev && d.id === dev.id ? ' selected' : '') + '>' + escapeHTML((d.name || d.id) + ' (' + d.backend + ' · ' + d.status + ')') + '</option>').join('')
      : '<option value="">No devices discovered</option>';

    body.innerHTML = [
      '<div class="devices-pane">',
      '  <div class="device-toolbar">',
      '    <select id="deviceSelect" aria-label="Android device">' + options + '</select>',
      '    <button id="refreshDevicesBtn" class="secondary" type="button">Refresh</button>',
      '    <button id="startDeviceBtn" class="secondary" type="button">Start AVD</button>',
      '    <button id="stopDeviceBtn" class="secondary" type="button"' + (dev && (dev.status === 'ready' || dev.status === 'running' || dev.status === 'starting') ? '' : ' disabled') + '>Stop</button>',
      '  </div>',
      '  <div class="device-meta-row">',
      '    <span>' + escapeHTML(dev ? ((dev.width || 1080) + '×' + (dev.height || 1920) + (dev.serial ? ' · ' + dev.serial : '')) : 'Plug in ADB USB/TCP device or create an AVD') + '</span>',
      '    <span id="deviceStreamStatus" class="device-stream-status">' + escapeHTML(state.deviceStreamStatus || (dev ? dev.status : 'idle')) + '</span>',
      '  </div>',
      '  <div id="deviceScreenStage" class="device-screen-stage">',
      '    <canvas class="device-screen-canvas" hidden></canvas>',
      '    <img class="device-screen-img" alt="Android screen stream" hidden />',
      '    <div class="device-screen-empty">' + escapeHTML(dev && (dev.status === 'ready' || dev.status === 'running') ? 'Waiting for frame...' : 'Device offline. Click Start AVD or connect an ADB device.') + '</div>',
      '  </div>',
      '  <div class="device-hw-bar">',
      '    <button id="devBackBtn" class="secondary" type="button"' + (!dev ? ' disabled' : '') + '>Back</button>',
      '    <button id="devHomeBtn" class="secondary" type="button"' + (!dev ? ' disabled' : '') + '>Home</button>',
      '    <button id="devAppSwitchBtn" class="secondary" type="button"' + (!dev ? ' disabled' : '') + '>Overview</button>',
      '    <button id="devShotBtn" class="secondary" type="button"' + (!dev ? ' disabled' : '') + '>Screenshot</button>',
      '    <button id="devLogsBtn" class="secondary" type="button"' + (!dev ? ' disabled' : '') + '>Logcat</button>',
      '  </div>',
      state.deviceLogs ? '<pre class="device-logcat custom-scrollbar">' + escapeHTML(state.deviceLogs) + '</pre>' : '',
      '</div>',
    ].join('\n');

    $('deviceSelect').onchange = () => {
      state.selectedDeviceID = $('deviceSelect').value;
      state.deviceLogs = '';
      closeDeviceStream();
      renderDevicesPane(body);
    };
    $('refreshDevicesBtn').onclick = async () => {
      await loadDevices();
      renderDevicesPane(body);
    };
    $('startDeviceBtn').onclick = async () => {
      if (!state.project) return;
      try {
        const target = selectedDeviceRecord();
        const payload = target && target.backend === 'avd' ? { backend: 'avd', avd_name: target.avd_name, device_id: target.id } : { backend: 'avd' };
        const started = await api('/api/projects/' + encodeURIComponent(state.project.id) + '/devices', { method: 'POST', body: JSON.stringify(payload) });
        state.selectedDeviceID = started.id || state.selectedDeviceID;
        await loadDevices();
        renderDevicesPane(body);
        notify('Android device start requested.', 'success');
      } catch (err) {
        notify(err.message || String(err), 'error');
      }
    };
    $('stopDeviceBtn').onclick = async () => {
      if (!state.project || !dev) return;
      try {
        await api('/api/projects/' + encodeURIComponent(state.project.id) + '/devices/' + encodeURIComponent(dev.id) + '/stop', { method: 'POST', body: '{}' });
        closeDeviceStream();
        await loadDevices();
        renderDevicesPane(body);
        notify('Device stopped.', 'success');
      } catch (err) {
        notify(err.message || String(err), 'error');
      }
    };
    $('devBackBtn').onclick = () => sendDeviceAction(dev, { kind: 'back' }).catch(err => notify(err.message || String(err), 'error'));
    $('devHomeBtn').onclick = () => sendDeviceAction(dev, { kind: 'home' }).catch(err => notify(err.message || String(err), 'error'));
    $('devAppSwitchBtn').onclick = () => sendDeviceAction(dev, { kind: 'app_switch' }).catch(err => notify(err.message || String(err), 'error'));
    $('devShotBtn').onclick = async () => {
      if (!state.project || !dev) return;
      try {
        const resp = await fetch('/api/projects/' + encodeURIComponent(state.project.id) + '/devices/' + encodeURIComponent(dev.id) + '/screenshot');
        if (!resp.ok) throw new Error(await resp.text());
        const buf = new Uint8Array(await resp.arrayBuffer());
        renderPNGFrameToSurface(buf);
      } catch (err) {
        notify(err.message || String(err), 'error');
      }
    };
    $('devLogsBtn').onclick = async () => {
      if (!state.project || !dev) return;
      try {
        const res = await api('/api/projects/' + encodeURIComponent(state.project.id) + '/devices/' + encodeURIComponent(dev.id) + '/logs?lines=120');
        state.deviceLogs = (res && res.logs) ? res.logs : '(empty logcat)';
        renderDevicesPane(body);
      } catch (err) {
        notify(err.message || String(err), 'error');
      }
    };

    const stage = $('deviceScreenStage');
    const resolveStageCoords = (event) => {
      if (!dev || (dev.status !== 'ready' && dev.status !== 'running')) return null;
      const canvas = stage.querySelector('.device-screen-canvas');
      const img = stage.querySelector('.device-screen-img');
      const surface = (canvas && !canvas.hidden ? canvas : img) || stage;
      return computeContainedTouchCoords(
        event.clientX,
        event.clientY,
        surface.getBoundingClientRect(),
        dev.width || 1080,
        dev.height || 1920
      );
    };
    const dispatchTap = (coords) => {
      if (activeDeviceSocket && activeDeviceSocket.readyState === 1) {
        activeDeviceSocket.send(JSON.stringify({
          type: 'touch',
          phase: 'tap',
          x: coords.x,
          y: coords.y,
          viewport_width: coords.width,
          viewport_height: coords.height,
        }));
      } else {
        void sendDeviceAction(dev, { kind: 'tap', x: coords.x, y: coords.y }).catch(() => {});
      }
    };
    const dispatchSwipe = (start, end, durationMS) => {
      const action = {
        kind: 'swipe',
        from_x: start.x,
        from_y: start.y,
        to_x: end.x,
        to_y: end.y,
        duration_ms: durationMS,
      };
      if (activeDeviceSocket && activeDeviceSocket.readyState === 1) {
        activeDeviceSocket.send(JSON.stringify({ type: 'action', action }));
      } else {
        void sendDeviceAction(dev, action).catch(() => {});
      }
    };

    let dragStart = null;
    let handledPointerGesture = false;
    stage.onpointerdown = (event) => {
      const coords = resolveStageCoords(event);
      if (!coords) {
        dragStart = null;
        return;
      }
      dragStart = {
        pointerId: event.pointerId,
        x: coords.x,
        y: coords.y,
        startedAt: Date.now(),
      };
      if (typeof stage.setPointerCapture === 'function') {
        try { stage.setPointerCapture(event.pointerId); } catch (_) {}
      }
    };
    stage.onpointercancel = () => {
      dragStart = null;
    };
    stage.onpointerup = (event) => {
      if (!dragStart || event.pointerId !== dragStart.pointerId) return;
      const start = dragStart;
      dragStart = null;
      const coords = resolveStageCoords(event);
      if (!coords) return;
      handledPointerGesture = true;
      setTimeout(() => { handledPointerGesture = false; }, 0);
      const distance = Math.hypot(coords.x - start.x, coords.y - start.y);
      if (distance >= 16) {
        const durationMS = Math.max(120, Math.min(800, Date.now() - start.startedAt));
        dispatchSwipe(start, coords, durationMS);
      } else {
        dispatchTap(coords);
      }
    };
    stage.onclick = (event) => {
      if (handledPointerGesture) return;
      const coords = resolveStageCoords(event);
      if (!coords) return;
      dispatchTap(coords);
    };

    connectDeviceStream(dev);
  }

  const exportsObj = {
    computeContainedTouchCoords,
    isPNGFrame,
    base64ToUint8Array,
    hasAnnexBKeyFrame,
  };
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = exportsObj;
  } else if (typeof window !== 'undefined') {
    window.loadDevices = loadDevices;
    window.openDevicesPane = openDevicesPane;
    window.closeDeviceStream = closeDeviceStream;
    window.renderDevicesPane = renderDevicesPane;
    window.__karozDevicesTest = exportsObj;
    window.KarozDevicesHelpers = exportsObj;
  }
})();
