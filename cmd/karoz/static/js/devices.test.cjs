const assert = require('node:assert/strict');
const test = require('node:test');
const {
  computeContainedTouchCoords,
  isPNGFrame,
  base64ToUint8Array,
  hasAnnexBKeyFrame,
} = require('./devices.js');

test('computeContainedTouchCoords accounts for object-contain pillarbox and letterbox offsets', () => {
  // Portrait 1080x1920 rendered inside square 400x400 container -> rendered width = 225, left offset = 87.5
  const rect = { left: 100, top: 50, width: 400, height: 400 };
  // Clicking in the left black pillarbox bar must return null
  assert.equal(computeContainedTouchCoords(120, 250, rect, 1080, 1920), null);

  // Clicking exact center should map near (540, 960)
  const center = computeContainedTouchCoords(300, 250, rect, 1080, 1920);
  assert.ok(center);
  assert.equal(center.width, 1080);
  assert.equal(center.height, 1920);
  assert.equal(center.x, 540);
  assert.equal(center.y, 960);
});

test('isPNGFrame distinguishes PNG frames from Annex-B H264 NAL units', () => {
  assert.equal(isPNGFrame(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00])), true);
  assert.equal(isPNGFrame(new Uint8Array([0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xc0, 0x1e])), false);
  assert.equal(isPNGFrame(new Uint8Array([0x89, 0x50])), false);
});

test('base64ToUint8Array decodes standard base64 payloads', () => {
  const bytes = base64ToUint8Array('AQIDBA==');
  assert.deepEqual(Array.from(bytes), [1, 2, 3, 4]);
});

test('hasAnnexBKeyFrame detects IDR frames prefixed with SPS and PPS NAL units', () => {
  const spsPpsIdr = new Uint8Array([
    0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xc0, 0x1e,
    0x00, 0x00, 0x00, 0x01, 0x68, 0xce, 0x3c, 0x80,
    0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x21,
  ]);
  assert.equal(hasAnnexBKeyFrame(spsPpsIdr), true);

  const threeByteIdr = new Uint8Array([0x00, 0x00, 0x01, 0x65, 0x88]);
  assert.equal(hasAnnexBKeyFrame(threeByteIdr), true);

  const nonIdrSlice = new Uint8Array([0x00, 0x00, 0x00, 0x01, 0x41, 0x9a, 0x22]);
  assert.equal(hasAnnexBKeyFrame(nonIdrSlice), false);
});
