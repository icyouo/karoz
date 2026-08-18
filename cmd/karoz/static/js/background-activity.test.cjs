const assert = require('node:assert/strict');
const {
  backgroundDuration,
  backgroundBytes,
  monitorHasUnacknowledgedGaps,
  backgroundMonitorCapabilities,
  backgroundScriptProbeEditorPresentation,
} = require('./background-activity.js');

assert.equal(backgroundDuration(0), '0 ms');
assert.equal(backgroundDuration(1250), '1.3 s');
assert.equal(backgroundDuration(65_000), '1m 5s');
assert.equal(backgroundBytes(1536), '1.5 KiB');
assert.equal(monitorHasUnacknowledgedGaps({ source_gaps: {} }), false);
assert.equal(monitorHasUnacknowledgedGaps({
  source_gaps: {
    one: { gap_version: 3, acknowledged_gap_version: 2 },
  },
}), true);
assert.equal(monitorHasUnacknowledgedGaps({
  source_gaps: {
    one: { gap_version: 3, acknowledged_gap_version: 3 },
  },
}), false);

const unsupportedProbe = backgroundMonitorCapabilities({
  trigger: { kind: 'script_probe' },
}, false);
assert.deepEqual(unsupportedProbe, {
  canRunCheck: false,
  runCheckTitle: 'Temporary code probes are not supported in v1 on Windows',
  canResume: false,
  resumeTitle: 'Temporary code probes are not supported in v1 on Windows',
});
assert.equal(backgroundMonitorCapabilities({
  trigger: { kind: 'script_probe' },
}, true).canRunCheck, true);
assert.equal(backgroundMonitorCapabilities({
  trigger: { kind: 'script_probe' },
  error_code: 'unsupported_platform',
}, true).canResume, false);
assert.equal(backgroundMonitorCapabilities({
  trigger: { kind: 'runtime_event' },
}, true).canRunCheck, false);
assert.deepEqual(backgroundScriptProbeEditorPresentation({
  id: 'monitor-7',
  revision: 4,
  trigger: {
    kind: 'script_probe',
    revision: 3,
    probe_language: 'shell',
    interval_ms: 60_000,
    timeout_ms: 5_000,
  },
}), {
  optionLabel: 'Temporary code · immutable',
  summary: 'shell · every 1m 0s · timeout 5.0 s',
  binding: 'Bound to MonitorID monitor-7 · revision 3',
});

console.log('background activity helpers: ok');
