const assert = require('node:assert/strict');
const {
  backgroundDuration,
  backgroundBytes,
  monitorHasUnacknowledgedGaps,
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

console.log('background activity helpers: ok');
